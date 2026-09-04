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

// Package resume measures actor activation latency against the installed
// counter demo for one sandbox class: the ResumeActor round trip as the
// client sees it, and atelet's per-restore phase breakdown from its logs.
//
// It lives outside internal/e2e/suites so the standard e2e lanes never run
// it. hack/bench-resume-injection-kind.sh drives it once per arm of the
// --inject-egress-trust-bundle comparison; see README.md for running it by
// hand.
package resume

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"text/tabwriter"
	"time"

	"github.com/agent-substrate/substrate/internal/e2e"
	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	benchAtespace   = "ate-resume-bench"
	ateletNamespace = "ate-system"
	ateletSelector  = "app=atelet"
	ateletContainer = "atelet"
	restoreLogMsg   = "Restore timing breakdown"

	samplesCSV = "samples.csv"
	phasesCSV  = "phases.csv"
	summaryTxt = "summary.txt"
)

var (
	samplesHeader = []string{"label", "class", "kind", "actor", "iter", "ms"}
	phasesHeader  = []string{"label", "class", "kind", "actor", "phase", "ms"}
)

// config is read from the environment so the driver script can run the same
// test per arm without adding flags to the shared e2e harness.
type config struct {
	// label names the arm ("off", "on") and is recorded in every sample.
	label string
	// golden is how many fresh actors to create; each is timed on its first
	// resume, which restores the template's golden snapshot.
	golden int
	// cycles is how many pause/resume cycles to time on the first actor;
	// each resume restores the local snapshot the pause wrote.
	cycles int
	// results is the directory the CSVs and summary accumulate in across
	// arms and classes. Empty logs the summary and keeps nothing.
	results string
}

func configFromEnv(t *testing.T) config {
	t.Helper()
	cfg := config{
		label:   os.Getenv("BENCH_LABEL"),
		golden:  envInt(t, "BENCH_GOLDEN", 5),
		cycles:  envInt(t, "BENCH_CYCLES", 20),
		results: os.Getenv("BENCH_RESULTS_DIR"),
	}
	if cfg.label == "" {
		cfg.label = "unlabeled"
	}
	if cfg.golden < 1 {
		t.Fatal("BENCH_GOLDEN must be at least 1: the pause/resume cycles run on the first actor")
	}
	return cfg
}

func envInt(t *testing.T, key string, def int) int {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		t.Fatalf("%s=%q: %v", key, v, err)
	}
	return n
}

func TestResumeLatency(t *testing.T) {
	cfg := configFromEnv(t)
	ctx := context.Background()
	clients := e2e.GetClients()
	tmpl := e2e.CounterFixture()
	class := e2e.SandboxClass()
	if class == "" {
		class = "gvisor"
	}
	t.Logf("arm=%s class=%s template=%s/%s golden=%d cycles=%d", cfg.label, class, tmpl.Namespace, tmpl.Name, cfg.golden, cfg.cycles)

	e2e.WaitForTemplateReady(ctx, t, clients, tmpl.Namespace, tmpl.Name)
	_, _ = clients.SubstrateAPI.CreateAtespace(ctx, &ateapipb.CreateAtespaceRequest{
		Atespace: &ateapipb.Atespace{Metadata: &ateapipb.ResourceMetadata{Name: benchAtespace}},
	})

	// The log window is padded because the node clock may lead this one.
	since := metav1.NewTime(time.Now().Add(-time.Minute))
	prefix := fmt.Sprintf("bench-%s-%d-", class, time.Now().Unix()%1_000_000)

	var samples [][]string
	for i := range cfg.golden {
		name := fmt.Sprintf("%s%d", prefix, i)
		createActor(t, ctx, clients, tmpl, name)
		samples = append(samples, sampleRow(cfg, class, "golden", name, i, timedResume(t, ctx, clients, name)))
		if i == 0 {
			for j := range cfg.cycles {
				pauseActor(t, ctx, clients, name)
				samples = append(samples, sampleRow(cfg, class, "local", name, j, timedResume(t, ctx, clients, name)))
			}
		}
		// Free the worker before the next actor so no timed call waits on
		// pool capacity.
		releaseActor(t, ctx, clients, name)
	}

	phases := ateletPhases(t, ctx, clients, since, cfg, class, prefix)
	report(t, cfg, samples, phases)
}

func sampleRow(cfg config, class, kind, actor string, iter int, ms float64) []string {
	return []string{cfg.label, class, kind, actor, strconv.Itoa(iter), strconv.FormatFloat(ms, 'f', 3, 64)}
}

func ref(name string) *ateapipb.ObjectRef {
	return &ateapipb.ObjectRef{Atespace: benchAtespace, Name: name}
}

func createActor(t *testing.T, ctx context.Context, clients *e2e.Clients, tmpl e2e.Fixture, name string) {
	t.Helper()
	if _, err := clients.SubstrateAPI.CreateActor(ctx, &ateapipb.CreateActorRequest{Actor: &ateapipb.Actor{
		Metadata:               &ateapipb.ResourceMetadata{Atespace: benchAtespace, Name: name},
		ActorTemplateNamespace: tmpl.Namespace,
		ActorTemplateName:      tmpl.Name,
	}}); err != nil {
		t.Fatalf("CreateActor %s: %v", name, err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_, _ = clients.SubstrateAPI.SuspendActor(cctx, &ateapipb.SuspendActorRequest{Actor: ref(name)})
		_, _ = clients.SubstrateAPI.DeleteActor(cctx, &ateapipb.DeleteActorRequest{Actor: ref(name), AnyState: true})
	})
	waitForState(t, ctx, clients, name, ateapipb.ActorState_ACTOR_STATE_SUSPENDED)
}

// timedResume returns the milliseconds one successful ResumeActor took.
// ResumeActor returns only after atelet's Restore and the RUNNING commit, so
// the round trip is the activation time. A saturated pool (ResourceExhausted)
// is waited out untimed and the call retried.
func timedResume(t *testing.T, ctx context.Context, clients *e2e.Clients, name string) float64 {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		start := time.Now()
		_, err := clients.SubstrateAPI.ResumeActor(ctx, &ateapipb.ResumeActorRequest{Actor: ref(name)})
		elapsed := time.Since(start)
		if err == nil {
			waitForState(t, ctx, clients, name, ateapipb.ActorState_ACTOR_STATE_RUNNING)
			return float64(elapsed) / float64(time.Millisecond)
		}
		if status.Code(err) != codes.ResourceExhausted || time.Now().After(deadline) {
			t.Fatalf("ResumeActor %s: %v", name, err)
		}
		t.Logf("ResumeActor %s: pool saturated (%v); retrying untimed", name, err)
		time.Sleep(2 * time.Second)
	}
}

func pauseActor(t *testing.T, ctx context.Context, clients *e2e.Clients, name string) {
	t.Helper()
	if _, err := clients.SubstrateAPI.PauseActor(ctx, &ateapipb.PauseActorRequest{Actor: ref(name)}); err != nil {
		t.Fatalf("PauseActor %s: %v", name, err)
	}
	waitForState(t, ctx, clients, name, ateapipb.ActorState_ACTOR_STATE_PAUSED)
}

func releaseActor(t *testing.T, ctx context.Context, clients *e2e.Clients, name string) {
	t.Helper()
	if _, err := clients.SubstrateAPI.SuspendActor(ctx, &ateapipb.SuspendActorRequest{Actor: ref(name)}); err != nil {
		t.Fatalf("SuspendActor %s: %v", name, err)
	}
	waitForState(t, ctx, clients, name, ateapipb.ActorState_ACTOR_STATE_SUSPENDED)
	if _, err := clients.SubstrateAPI.DeleteActor(ctx, &ateapipb.DeleteActorRequest{Actor: ref(name)}); err != nil {
		t.Fatalf("DeleteActor %s: %v", name, err)
	}
}

func waitForState(t *testing.T, ctx context.Context, clients *e2e.Clients, name string, want ateapipb.ActorState) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		resp, err := clients.SubstrateAPI.GetActor(ctx, &ateapipb.GetActorRequest{Actor: ref(name)})
		if err == nil && resp.GetStatus().GetState() == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("actor %s never reached %v (last state %v, err %v)", name, want, resp.GetStatus().GetState(), err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// ateletPhases reads atelet's restore timing log lines for this run's actors.
// Every numeric field on the line is a slog.Duration (JSON nanoseconds), so
// each becomes a phase sample and the set follows whatever atelet logs on the
// branch under test. Per actor, the first restore in time order is the golden
// one; the rest are local restores from pause/resume cycles.
func ateletPhases(t *testing.T, ctx context.Context, clients *e2e.Clients, since metav1.Time, cfg config, class, prefix string) [][]string {
	t.Helper()
	pods, err := clients.K8s.CoreV1().Pods(ateletNamespace).List(ctx, metav1.ListOptions{LabelSelector: ateletSelector})
	if err != nil {
		t.Fatalf("listing atelet pods: %v", err)
	}
	actorRE := regexp.MustCompile(regexp.QuoteMeta(prefix) + `\d+`)

	type restoreLine struct {
		at     time.Time
		actor  string
		phases map[string]float64
	}
	var lines []restoreLine
	for _, pod := range pods.Items {
		stream, err := clients.K8s.CoreV1().Pods(ateletNamespace).GetLogs(pod.Name, &corev1.PodLogOptions{
			Container: ateletContainer,
			SinceTime: &since,
		}).Stream(ctx)
		if err != nil {
			t.Fatalf("streaming logs of %s: %v", pod.Name, err)
		}
		sc := bufio.NewScanner(stream)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			raw := sc.Bytes()
			if !bytes.Contains(raw, []byte(restoreLogMsg)) || !bytes.Contains(raw, []byte(prefix)) {
				continue
			}
			var rec map[string]any
			if err := json.Unmarshal(raw, &rec); err != nil || rec["msg"] != restoreLogMsg {
				continue
			}
			actorJSON, _ := json.Marshal(rec["actor"])
			actor := actorRE.FindString(string(actorJSON))
			if actor == "" {
				continue
			}
			at, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(rec["time"]))
			phases := map[string]float64{}
			collectDurations("", rec, phases)
			lines = append(lines, restoreLine{at: at, actor: actor, phases: phases})
		}
		_ = stream.Close()
		if err := sc.Err(); err != nil {
			t.Fatalf("reading logs of %s: %v", pod.Name, err)
		}
	}
	if len(lines) == 0 {
		t.Logf("no %q lines for %s* in atelet logs; node-side breakdown unavailable", restoreLogMsg, prefix)
		return nil
	}

	sort.SliceStable(lines, func(i, j int) bool { return lines[i].at.Before(lines[j].at) })
	seen := map[string]int{}
	var rows [][]string
	for _, l := range lines {
		kind := "local"
		if seen[l.actor] == 0 {
			kind = "golden"
		}
		seen[l.actor]++
		for phase, ms := range l.phases {
			rows = append(rows, []string{cfg.label, class, kind, l.actor, phase, strconv.FormatFloat(ms, 'f', 3, 64)})
		}
	}
	return rows
}

// collectDurations flattens the numeric fields of a log record into phases
// in milliseconds, descending one level into groups. The actor field is a
// reference, not a timing.
func collectDurations(prefix string, rec map[string]any, out map[string]float64) {
	for k, v := range rec {
		switch val := v.(type) {
		case float64:
			out[prefix+k] = val / float64(time.Millisecond)
		case map[string]any:
			if prefix == "" && k == "actor" {
				continue
			}
			collectDurations(prefix+k+".", val, out)
		}
	}
}

// report logs this run's tables and, with a results directory, appends the
// rows to the CSVs and rewrites summary.txt over every arm and class
// recorded there so far.
func report(t *testing.T, cfg config, samples, phases [][]string) {
	t.Helper()
	if cfg.results == "" {
		t.Logf("\n%s", render(samples, phases))
		return
	}
	if err := os.MkdirAll(cfg.results, 0o755); err != nil {
		t.Fatalf("creating %s: %v", cfg.results, err)
	}
	appendCSV(t, filepath.Join(cfg.results, samplesCSV), samplesHeader, samples)
	appendCSV(t, filepath.Join(cfg.results, phasesCSV), phasesHeader, phases)

	all := render(readCSV(t, filepath.Join(cfg.results, samplesCSV)), readCSV(t, filepath.Join(cfg.results, phasesCSV)))
	if err := os.WriteFile(filepath.Join(cfg.results, summaryTxt), []byte(all), 0o644); err != nil {
		t.Fatalf("writing summary: %v", err)
	}
	t.Logf("\n%s", all)
}

func appendCSV(t *testing.T, path string, header []string, rows [][]string) {
	t.Helper()
	if len(rows) == 0 {
		return
	}
	_, statErr := os.Stat(path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	defer f.Close()
	w := csv.NewWriter(f)
	if os.IsNotExist(statErr) {
		_ = w.Write(header)
	}
	_ = w.WriteAll(rows)
	if err := w.Error(); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func readCSV(t *testing.T, path string) [][]string {
	t.Helper()
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if len(rows) > 0 {
		rows = rows[1:]
	}
	return rows
}

// render prints one table per CSV: client round trips grouped by class,
// kind and label; atelet phases grouped by class, kind, phase and label.
// Label is the innermost group so the arms sit on adjacent rows.
func render(samples, phases [][]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "ResumeActor round trip, client side (ms)\n")
	b.WriteString(table(samples, []int{1, 2, 0}, []string{"class", "kind", "label"}, 5))
	if len(phases) > 0 {
		fmt.Fprintf(&b, "\natelet restore phases (ms)\n")
		b.WriteString(table(phases, []int{1, 2, 4, 0}, []string{"class", "kind", "phase", "label"}, 5))
	}
	return b.String()
}

func table(rows [][]string, keyCols []int, keyNames []string, valCol int) string {
	groups := map[string][]float64{}
	for _, r := range rows {
		if len(r) <= valCol {
			continue
		}
		v, err := strconv.ParseFloat(r[valCol], 64)
		if err != nil {
			continue
		}
		var key []string
		for _, c := range keyCols {
			key = append(key, r[c])
		}
		k := strings.Join(key, "\x00")
		groups[k] = append(groups[k], v)
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "%s\tn\tp50\tp90\tmax\tmean\n", strings.Join(keyNames, "\t"))
	for _, k := range keys {
		s := summarize(groups[k])
		fmt.Fprintf(w, "%s\t%d\t%.1f\t%.1f\t%.1f\t%.1f\n", strings.ReplaceAll(k, "\x00", "\t"), s.n, s.p50, s.p90, s.max, s.mean)
	}
	_ = w.Flush()
	return b.String()
}

type stats struct {
	n                   int
	p50, p90, max, mean float64
}

func summarize(v []float64) stats {
	sorted := append([]float64(nil), v...)
	sort.Float64s(sorted)
	var sum float64
	for _, x := range sorted {
		sum += x
	}
	pct := func(p float64) float64 {
		i := int(p*float64(len(sorted))+0.999999) - 1
		return sorted[max(0, min(i, len(sorted)-1))]
	}
	return stats{n: len(sorted), p50: pct(0.5), p90: pct(0.9), max: sorted[len(sorted)-1], mean: sum / float64(len(sorted))}
}
