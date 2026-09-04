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

package controlapi

import (
	"context"
	"testing"

	"github.com/agent-substrate/substrate/internal/proto/ateletpb"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
)

// benchSpec is the counter demo's shape: two containers, one durable volume.
// Rebuilt per iteration because injection appends to it in place; the
// build-only sub-benchmark is the baseline to subtract.
func benchSpec() *ateletpb.WorkloadSpec {
	return &ateletpb.WorkloadSpec{
		Containers: []*ateletpb.Container{
			{Name: "main", VolumeMounts: []*ateletpb.VolumeMount{{Name: "data", MountPath: "/data"}}},
			{Name: "sidecar"},
		},
		Volumes: []*ateletpb.Volume{
			{Name: "data", Source: &ateletpb.Volume_DurableDir{DurableDir: &ateletpb.DurableDirVolume{}}},
		},
	}
}

// benchSink keeps the baseline spec on the heap so build-spec and
// build-spec+inject allocate alike and the difference is the injection.
var benchSink *ateletpb.WorkloadSpec

// BenchmarkInjectEgressTrustVolume: the control-plane side of
// --inject-egress-trust-bundle, run once per spec build.
//
//	go test ./cmd/ateapi/internal/controlapi -run '^$' -bench InjectEgressTrust -benchmem
func BenchmarkInjectEgressTrustVolume(b *testing.B) {
	ctx := context.Background()
	actor := &ateapipb.Actor{Metadata: &ateapipb.ResourceMetadata{Atespace: "bench", Name: "actor"}}
	b.Run("build-spec", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			benchSink = benchSpec()
		}
	})
	b.Run("build-spec+inject", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			benchSink = benchSpec()
			if err := injectEgressTrustVolume(ctx, actor, benchSink); err != nil {
				b.Fatal(err)
			}
		}
	})
}
