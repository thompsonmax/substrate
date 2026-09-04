// Copyright 2026 Google LLC
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

package main

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/agent-substrate/substrate/internal/proto/ateletpb"
	"github.com/agent-substrate/substrate/internal/resources"
	certsv1beta1 "k8s.io/api/certificates/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	certlisters "k8s.io/client-go/listers/certificates/v1beta1"
)

// Node-side cost of the volume ateapi injects under
// --inject-egress-trust-bundle. Both benchmarks run on the Run/Restore path
// once per activation; the second is the whole of it (prepareOCIBundles calls
// writeSystemInfoVolume, nothing else is added on the node).
//
//	go test ./cmd/atelet -run '^$' -bench 'TrustBundle|SystemInfoVolume' -benchmem
//
// TMPDIR selects the filesystem the write lands on; point it at the disk
// backing the node's actor directories for representative rename costs.

// benchAnchorCounts spans a single MITM CA through a rotation-sized bundle.
var benchAnchorCounts = []int{1, 3, 8}

func benchBundleLister(b *testing.B, anchors int) certlisters.ClusterTrustBundleLister {
	var bundle bytes.Buffer
	for range anchors {
		bundle.Write(testCertPEM(b))
	}
	return ctbLister(b, &certsv1beta1.ClusterTrustBundle{
		ObjectMeta: metav1.ObjectMeta{Name: egressTrustBundleObjectName},
		Spec:       certsv1beta1.ClusterTrustBundleSpec{TrustBundle: bundle.String()},
	})
}

// BenchmarkResolveTrustBundle: informer-cache Get plus PEM sanitization
// (parse, dedupe, shuffle). No API round trip is involved.
func BenchmarkResolveTrustBundle(b *testing.B) {
	for _, anchors := range benchAnchorCounts {
		b.Run(fmt.Sprintf("anchors=%d", anchors), func(b *testing.B) {
			lister := benchBundleLister(b, anchors)
			b.ReportAllocs()
			for b.Loop() {
				if _, err := resolveTrustBundle(lister, EgressTrustBundleName); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkWriteSystemInfoVolume: resolve, then write-to-temp-and-rename the
// projected file under the volume root, over the previous activation's copy
// as on every resume.
func BenchmarkWriteSystemInfoVolume(b *testing.B) {
	si := &ateletpb.SystemInfoVolume{
		DataSources: []*ateletpb.SystemInfoDataSource{{
			DataSource: &ateletpb.SystemInfoDataSource_TrustBundle{
				TrustBundle: &ateletpb.TrustBundleDataSource{Name: EgressTrustBundleName, Path: "egress-mitm.ate.dev.pem"},
			},
		}},
	}
	ref := resources.ActorRef{Atespace: "bench", Name: "actor"}
	ctx := context.Background()
	for _, anchors := range benchAnchorCounts {
		b.Run(fmt.Sprintf("anchors=%d", anchors), func(b *testing.B) {
			lister := benchBundleLister(b, anchors)
			root := filepath.Join(b.TempDir(), "system-info", "trust.ate.dev")
			b.ReportAllocs()
			for b.Loop() {
				if err := writeSystemInfoVolume(ctx, root, ref, "uid", lister, si); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
