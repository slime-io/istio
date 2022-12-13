// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package apiserver

import (
	"os"
	"strings"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	"istio.io/istio-lib/galley/pkg/config/scope"
	"istio.io/istio-lib/galley/pkg/config/source/kube/apiserver/stats"
	"istio.io/istio-lib/galley/pkg/config/source/kube/apiserver/tombstone"
	"istio.io/istio-lib/galley/pkg/config/source/kube/rt"
	"istio.io/istio-lib/pkg/config/event"
	"istio.io/istio-lib/pkg/config/schema/collection"
)

type watcher struct {
	mu sync.Mutex

	adapter *rt.Adapter
	schema  collection.Schema

	handler event.Handler

	done chan struct{}
	rev  string
	crds map[string]string
}

func newWatcher(r collection.Schema, a *rt.Adapter) *watcher {
	rev := os.Getenv("istio-revision")
	crds := os.Getenv("rev-crds")
	items := strings.Split(crds, ",")
	existCrds := make(map[string]string, 0)
	for _, item := range items {
		existCrds[item] = item
	}
	return &watcher{
		schema:  r,
		adapter: a,
		handler: event.SentinelHandler(),
		rev:     rev,
		crds:    existCrds,
	}
}

func (w *watcher) start() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done != nil {
		panic("watcher.start: already started")
	}

	scope.Source.Debugf("Starting watcher for %q (%q)", w.schema.Name(), w.schema.Resource().GroupVersionKind())

	informer, err := w.adapter.NewInformer()
	if err != nil {
		scope.Source.Errorf("unable to start watcher for %q: %v", w.schema.Resource().GroupVersionKind(), err)
		// Send a FullSync event, even if the informer is not available. This will ensure that the processing backend
		// will still work, in absence of CRDs.
		w.handler.Handle(event.FullSyncFor(w.schema))
		return
	}

	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) { w.handleEvent(event.Added, obj) },
		UpdateFunc: func(old, new interface{}) {
			if w.adapter.IsEqual(old, new) {
				// Periodic resync will send update events for all known resources.
				// Two different versions of the same resource will always have different RVs.
				return
			}

			w.handleEvent(event.Updated, new)
		},
		DeleteFunc: func(obj interface{}) { w.handleEvent(event.Deleted, obj) },
	})

	done := make(chan struct{})
	w.done = done

	// Start CRD shared informer and wait for it to exit.
	go informer.Run(done)
	// Send the FullSync event after the cache syncs.
	if cache.WaitForCacheSync(done, informer.HasSynced) {
		go w.handler.Handle(event.FullSyncFor(w.schema))
	}
}

func (w *watcher) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done != nil {
		close(w.done)
		w.done = nil
	}
}

func (w *watcher) dispatch(h event.Handler) {
	w.handler = event.CombineHandlers(w.handler, h)
}

func (w *watcher) handleEvent(c event.Kind, obj interface{}) {
	object, ok := obj.(metav1.Object)
	if !ok {
		if obj = tombstone.RecoverResource(obj); object != nil {
			// Tombstone recovery failed.
			scope.Source.Warnf("Unable to extract object for event: %v", obj)
			return
		}
		obj = object
	}

	object = w.adapter.ExtractObject(obj)
	res, err := w.adapter.ExtractResource(obj)
	if err != nil {
		scope.Source.Warnf("unable to extract resource: %v: %e", obj, err)
		return
	}
	r := rt.ToResource(object, w.schema, res, nil)
	skip := false
	_, exist := w.crds[r.Metadata.Schema.Plural()]
	if exist {
		// empty watcher rev: accept all
		// non-empty watcher rev: accept resource with empty rev or same rev
		if w.rev != "" && r.Metadata.Labels != nil {
			rRev := r.Metadata.Labels["istio.io/rev"]
			if rRev != "" && rRev != w.rev {
				skip = true
				scope.Source.Warnf("istio.io/rev not match skip resource: %v", r.Metadata.FullName)
			}
		}
	}
	if skip {
		switch c {
		case event.Updated:
			c = event.Deleted
		case event.Deleted:

		default:
			return
		}
	}

	e := event.Event{
		Kind:     c,
		Source:   w.schema,
		Resource: r,
	}

	w.handler.Handle(e)

	stats.RecordEventSuccess()
}
