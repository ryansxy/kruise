/*
Copyright 2020 The Kruise Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"sync"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	v1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apiextensionsinformers "k8s.io/apiextensions-apiserver/pkg/client/informers/externalversions/apiextensions/v1"
	apiextensionslisters "k8s.io/apiextensions-apiserver/pkg/client/listers/apiextensions/v1"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	admissionregistrationinformers "k8s.io/client-go/informers/admissionregistration/v1"
	coreinformers "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	extclient "github.com/openkruise/kruise/pkg/client"
	webhookutil "github.com/openkruise/kruise/pkg/webhook/util"
	"github.com/openkruise/kruise/pkg/webhook/util/configuration"
	"github.com/openkruise/kruise/pkg/webhook/util/crd"
	"github.com/openkruise/kruise/pkg/webhook/util/generator"
	"github.com/openkruise/kruise/pkg/webhook/util/writer"
)

const (
	mutatingWebhookConfigurationName   = "kruise-mutating-webhook-configuration"
	validatingWebhookConfigurationName = "kruise-validating-webhook-configuration"

	defaultResyncPeriod = time.Minute
)

var (
	namespace  = webhookutil.GetNamespace()
	secretName = webhookutil.GetSecretName()

	uninit   = make(chan struct{})
	onceInit = sync.Once{}
)

func Inited() chan struct{} {
	return uninit
}

type Controller struct {
	kubeClient clientset.Interface
	handlers   map[string]admission.Handler

	informerFactory informers.SharedInformerFactory
	crdClient       apiextensionsclientset.Interface
	crdInformer     cache.SharedIndexInformer
	crdLister       apiextensionslisters.CustomResourceDefinitionLister
	synced          []cache.InformerSynced

	queue workqueue.RateLimitingInterface
}

func New(cfg *rest.Config, handlers map[string]admission.Handler) (*Controller, error) {
	c := &Controller{
		kubeClient: extclient.GetGenericClientWithName("webhook-controller").KubeClient,
		handlers:   handlers,
		queue:      workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "webhook-controller"),
	}

	c.informerFactory = informers.NewSharedInformerFactory(c.kubeClient, 0)

	secretInformer := coreinformers.New(c.informerFactory, namespace, nil).Secrets()
	admissionRegistrationInformer := admissionregistrationinformers.New(c.informerFactory, v1.NamespaceAll, nil)

	// 1.secret Handler
	// 只关心和自己相关的 secretName(可能为kruise-webhook-certs) 的 add 和 update,入queue
	secretInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			secret := obj.(*v1.Secret)
			if secret.Name == secretName {
				klog.Infof("Secret %s added", secretName)
				c.queue.Add("")
			}
		},
		UpdateFunc: func(old, cur interface{}) {
			secret := cur.(*v1.Secret)
			if secret.Name == secretName {
				klog.Infof("Secret %s updated", secretName)
				c.queue.Add("")
			}
		},
	})

	// 2.MutatingWebhookConfigurations Handler
	// 只关心和自己相关的 MutatingWebhookConfiguration Name(可能为 kruise-mutating-webhook-configuration) 的 add 和 update,入queue
	admissionRegistrationInformer.MutatingWebhookConfigurations().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			conf := obj.(*admissionregistrationv1.MutatingWebhookConfiguration)
			if conf.Name == mutatingWebhookConfigurationName {
				klog.Infof("MutatingWebhookConfiguration %s added", mutatingWebhookConfigurationName)
				c.queue.Add("")
			}
		},
		UpdateFunc: func(old, cur interface{}) {
			conf := cur.(*admissionregistrationv1.MutatingWebhookConfiguration)
			if conf.Name == mutatingWebhookConfigurationName {
				klog.Infof("MutatingWebhookConfiguration %s update", mutatingWebhookConfigurationName)
				c.queue.Add("")
			}
		},
	})

	// 3.ValidatingWebhookConfigurations Handler
	// 只关心和自己相关的 ValidatingWebhookConfigurations Name(可能为 kruise-validating-webhook-configuration) 的 add 和 update,入queue
	admissionRegistrationInformer.ValidatingWebhookConfigurations().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			conf := obj.(*admissionregistrationv1.ValidatingWebhookConfiguration)
			if conf.Name == validatingWebhookConfigurationName {
				klog.Infof("ValidatingWebhookConfiguration %s added", validatingWebhookConfigurationName)
				c.queue.Add("")
			}
		},
		UpdateFunc: func(old, cur interface{}) {
			conf := cur.(*admissionregistrationv1.ValidatingWebhookConfiguration)
			if conf.Name == validatingWebhookConfigurationName {
				klog.Infof("ValidatingWebhookConfiguration %s updated", validatingWebhookConfigurationName)
				c.queue.Add("")
			}
		},
	})

	// 4. crd Handler
	// 只关心crd.spc.group=apps.kruise.io" 的crd 的 add 和 update,入queue
	c.crdClient = apiextensionsclientset.NewForConfigOrDie(cfg)
	c.crdInformer = apiextensionsinformers.NewCustomResourceDefinitionInformer(c.crdClient, 0, cache.Indexers{})
	c.crdInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			crd := obj.(*apiextensionsv1.CustomResourceDefinition)
			if crd.Spec.Group == "apps.kruise.io" {
				klog.Infof("CustomResourceDefinition %s added", crd.Name)
				c.queue.Add("")
			}
		},
		UpdateFunc: func(old, cur interface{}) {
			crd := cur.(*apiextensionsv1.CustomResourceDefinition)
			if crd.Spec.Group == "apps.kruise.io" {
				klog.Infof("CustomResourceDefinition %s updated", crd.Name)
				c.queue.Add("")
			}
		},
	})
	// 5.设置crdLister: 通过crdInformer 的 indexer
	c.crdLister = apiextensionslisters.NewCustomResourceDefinitionLister(c.crdInformer.GetIndexer())

	c.synced = []cache.InformerSynced{
		secretInformer.Informer().HasSynced,
		admissionRegistrationInformer.MutatingWebhookConfigurations().Informer().HasSynced,
		admissionRegistrationInformer.ValidatingWebhookConfigurations().Informer().HasSynced,
		c.crdInformer.HasSynced,
	}

	return c, nil
}

func (c *Controller) Start(ctx context.Context) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	klog.Infof("Starting webhook-controller")
	defer klog.Infof("Shutting down webhook-controller")

	c.informerFactory.Start(ctx.Done()) // 1.启动 k8s  informerFactory
	go func() {
		c.crdInformer.Run(ctx.Done()) // 2.Run  crdInformer
	}()
	if !cache.WaitForNamedCacheSync("webhook-controller", ctx.Done(), c.synced...) {
		return
	}

	// 3. 每隔1s运行一次
	go wait.Until(func() {
		for c.processNextWorkItem() {
		}
	}, time.Second, ctx.Done())
	klog.Infof("Started webhook-controller")

	<-ctx.Done()
}

func (c *Controller) processNextWorkItem() bool {
	key, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(key)

	err := c.sync() // 1.如果同步成功，调用queue.AddAfter ,延迟1m入队，再次同步，即是定期同步。
	if err == nil {
		c.queue.AddAfter(key, defaultResyncPeriod)
		c.queue.Forget(key)
		return true
	}
	// 2.如果同步失败，则日志，并加入限速队列，再次重试,
	// 注意：失败的时候不用使用Forget(key)

	// Forget indicates that an item is finished being retried.  Doesn't matter whether it's for perm failing or for success,
	// we'll stop the rate limiter from tracking it.  This only clears the `rateLimiter`, you still have to call `Done` on the queue.
	// Forget 表示某item已完成重试。 不管是执行失败还是成功，
	// 我们将关闭 rate limiter 跟踪它。 这只会清除 `rateLimiter`，您仍然需要在队列上调用 `Done`。
	utilruntime.HandleError(fmt.Errorf("sync %q failed with %v", key, err))
	c.queue.AddRateLimited(key)

	return true
}

// 和 kruise 相关的secretName、MutatingWebhookConfigurations、validatingWebhookConfigurationName、以及 crd.spec.group=apps.kruise.io 的变更都会触发 入队reconcile
func (c *Controller) sync() error {
	klog.Infof("Starting to sync webhook certs and configurations")
	defer func() {
		klog.Infof("Finished to sync webhook certs and configurations")
	}()

	dnsName := webhookutil.GetHost() // 1.获取 WEBHOOK_HOST env 的值，如果不存在， 则 dnsName = ns.svcName.svc
	if len(dnsName) == 0 {
		dnsName = generator.ServiceToCommonName(webhookutil.GetNamespace(), webhookutil.GetServiceName())
	}

	var certWriter writer.CertWriter
	var err error

	certWriterType := webhookutil.GetCertWriter() // 2.获取 WEBHOOK_CERT_WRITER env 的值
	// 2.1 如果type=fs或type=""且WEBHOOK_HOST env 的存在,certWriter 会等于 fsCertWriter
	if certWriterType == writer.FsCertWriter || (len(certWriterType) == 0 && len(webhookutil.GetHost()) != 0) {
		certWriter, err = writer.NewFSCertWriter(writer.FSCertWriterOptions{
			Path: webhookutil.GetCertDir(), //  path="/tmp/kruise-webhook-certs"
		})
	} else {
		// 2.2 如果type!=fs或type=""但WEBHOOK_HOST env 的不存在，certWriter 会等于 secretCertWriter
		certWriter, err = writer.NewSecretCertWriter(writer.SecretCertWriterOptions{
			Clientset: c.kubeClient,
			Secret:    &types.NamespacedName{Namespace: webhookutil.GetNamespace(), Name: webhookutil.GetSecretName()}, //ns+secretName
		})
	}
	if err != nil {
		return fmt.Errorf("failed to ensure certs: %v", err)
	}

	// 3.EnsureCert 为 webhookClientConfig 提供证书。
	certs, _, err := certWriter.EnsureCert(dnsName)
	if err != nil {
		return fmt.Errorf("failed to ensure certs: %v", err)
	}
	// 4.将证书写入到 CertDir 中
	if err := writer.WriteCertsToDir(webhookutil.GetCertDir(), certs); err != nil {
		return fmt.Errorf("failed to write certs to dir: %v", err)
	}
	// 5.
	if err := configuration.Ensure(c.kubeClient, c.handlers, certs.CACert); err != nil {
		return fmt.Errorf("failed to ensure configuration: %v", err)
	}
	// 6.
	if err := crd.Ensure(c.crdClient, c.crdLister, certs.CACert); err != nil {
		return fmt.Errorf("failed to ensure crd: %v", err)
	}

	// 7.once.Do 函数只会执行一次，这里执行完会close  uninit 的 channel
	onceInit.Do(func() {
		close(uninit)
	})
	return nil
}
