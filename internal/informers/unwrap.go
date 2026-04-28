/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package informers

import (
	clientcache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// unwrapTombstone normalises a raw informer payload into a client.Object.
//
// On delete events, client-go may deliver a clientcache.DeletedFinalStateUnknown
// sentinel rather than the deleted object itself — this happens when the
// informer's watch is interrupted and a relist discovers the deletion after
// the fact. The sentinel carries the last-known object as Obj.
//
// The two paths (real object vs. tombstone) are merged here so the calling
// site doesn't have to repeat the type assertions inline.
func unwrapTombstone(raw any) (client.Object, bool) {
	if tombstone, ok := raw.(clientcache.DeletedFinalStateUnknown); ok {
		raw = tombstone.Obj
	}
	co, ok := raw.(client.Object)
	return co, ok
}
