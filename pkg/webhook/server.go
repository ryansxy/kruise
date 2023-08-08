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

package webhook

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	"sigs.k8s.io/controller-runtime/pkg/webhook/conversion"

	webhookutil "github.com/openkruise/kruise/pkg/webhook/util"
	webhookcontroller "github.com/openkruise/kruise/pkg/webhook/util/controller"
	"github.com/openkruise/kruise/pkg/webhook/util/health"
)

type GateFunc func() (enabled bool)

var (
	// HandlerMap contains all admission webhook handlers.
	HandlerMap   = map[string]admission.Handler{} // key=path,value=对应的Handler
	handlerGates = map[string]GateFunc{}          // key=path,value=是否开启，不开启的才会放这里
)

// 这个方法会被这个 package 下，所有的 add_ 开头的文件中 的 init() 方法调用
// 在 golang 中对于一个包下有多个文件，多个文件有多个init方法，并不是按照文件的顺序执行的，而是按照调用的顺序执行的。
func addHandlers(m map[string]admission.Handler) {
	addHandlersWithGate(m, nil)
}

func addHandlersWithGate(m map[string]admission.Handler, fn GateFunc) {
	for path, handler := range m {
		if len(path) == 0 {
			klog.Warningf("Skip handler with empty path.")
			continue
		}
		if path[0] != '/' { // 如果 path 的第一个字符不是/, 为其加上/
			path = "/" + path
		}
		_, found := HandlerMap[path]
		if found {
			klog.V(1).Infof("conflicting webhook builder path %v in handler map", path)
		}
		HandlerMap[path] = handler
		if fn != nil {
			handlerGates[path] = fn
		}
	}
}

// 过滤没有开启 的 path 和 其handler
func filterActiveHandlers() {
	disablePaths := sets.NewString()
	for path := range HandlerMap {
		if fn, ok := handlerGates[path]; ok {
			if !fn() {
				disablePaths.Insert(path)
			}
		}
	}
	for _, path := range disablePaths.List() {
		delete(HandlerMap, path)
	}
}

// 一个标准的使用 controller-runtime 实现的 webhook server
func SetupWithManager(mgr manager.Manager) error {
	server := mgr.GetWebhookServer()
	server.Host = "0.0.0.0"
	server.Port = webhookutil.GetPort()       // 获取 port, 考虑灵活，支持从 env 中读取
	server.CertDir = webhookutil.GetCertDir() // 获取 CertDir，支持从 env 中读取

	// register admission handlers
	filterActiveHandlers()
	for path, handler := range HandlerMap {
		server.Register(path, &webhook.Admission{Handler: handler}) // 为server 注册 path 和 handler
		klog.V(3).Infof("Registered webhook handler %s", path)
	}

	// register conversion webhook
	server.Register("/convert", &conversion.Webhook{}) // controller-runtime 默认的路径和Handler,用来解决啥？？

	// register health handler
	server.Register("/healthz", &health.Handler{})

	return nil
}

// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=mutatingwebhookconfigurations,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=validatingwebhookconfigurations,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch;update;patch

func Initialize(ctx context.Context, cfg *rest.Config) error {
	c, err := webhookcontroller.New(cfg, HandlerMap) // 1.启动 controller ,处理证书相关的事
	if err != nil {
		return err
	}
	go func() {
		c.Start(ctx)
	}()

	timer := time.NewTimer(time.Second * 20)
	defer timer.Stop()
	select {
	case <-webhookcontroller.Inited(): // 2.如果初始化完成，会close掉channel, 这里case会通过
		return nil
	case <-timer.C: // 3. 如果初始化未完成，每过20s进行一次
		return fmt.Errorf("failed to start webhook controller for waiting more than 20s")
	}
}

func Checker(req *http.Request) error {
	// Firstly wait webhook controller initialized
	select {
	case <-webhookcontroller.Inited():
	default:
		return fmt.Errorf("webhook controller has not initialized")
	}
	return health.Checker(req) // 真正发起 webhook 服务的 check 操作
}

// 等待 wenhook ready ，如果check后没有ready,sleep 2s 后会再次check
func WaitReady() error {
	startTS := time.Now()
	var err error
	for {
		duration := time.Since(startTS)
		if err = Checker(nil); err == nil {
			return nil
		}

		if duration > time.Second*5 {
			klog.Warningf("Failed to wait webhook ready over %s: %v", duration, err)
		}
		time.Sleep(time.Second * 2)
	}

}
