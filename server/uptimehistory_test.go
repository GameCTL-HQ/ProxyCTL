package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRequireConsecutiveFailuresNeedsTwoFailuresAndResetsOnSuccess(t *testing.T) {
	streaks := map[string]int{}
	states := map[string]bool{}

	first := map[string]UptimeSample{"route-1": {Reachable: false}}
	requireConsecutiveFailures(first, streaks, states, 2)
	if !first["route-1"].Reachable {
		t.Fatal("first failed check marked the target down; want it held up until a second consecutive failure")
	}
	if streaks["route-1"] != 1 {
		t.Fatalf("failure streak after first failure = %d, want 1", streaks["route-1"])
	}

	second := map[string]UptimeSample{"route-1": {Reachable: false}}
	requireConsecutiveFailures(second, streaks, states, 2)
	if second["route-1"].Reachable {
		t.Fatal("second consecutive failed check left the target up; want it marked down")
	}
	if streaks["route-1"] != 2 {
		t.Fatalf("failure streak after second failure = %d, want 2", streaks["route-1"])
	}

	recovered := map[string]UptimeSample{"route-1": {Reachable: true, LatencyMS: 12}}
	requireConsecutiveFailures(recovered, streaks, states, 2)
	if !recovered["route-1"].Reachable {
		t.Fatal("successful check did not immediately restore reachability")
	}
	if streaks["route-1"] != 0 {
		t.Fatalf("failure streak after recovery = %d, want 0", streaks["route-1"])
	}

	afterRecovery := map[string]UptimeSample{"route-1": {Reachable: false}}
	requireConsecutiveFailures(afterRecovery, streaks, states, 2)
	if !afterRecovery["route-1"].Reachable {
		t.Fatal("first failure after recovery marked the target down; want a fresh two-failure window")
	}
}

func TestRequireConsecutiveFailuresTracksTargetsIndependently(t *testing.T) {
	streaks := map[string]int{"wireguard-a": 1}
	states := map[string]bool{"wireguard-a": true}
	samples := map[string]UptimeSample{
		"wireguard-a": {Reachable: false},
		"wireguard-b": {Reachable: false},
	}

	requireConsecutiveFailures(samples, streaks, states, 2)

	if samples["wireguard-a"].Reachable {
		t.Fatal("second failure for wireguard-a did not mark it down")
	}
	if !samples["wireguard-b"].Reachable {
		t.Fatal("first failure for wireguard-b incorrectly inherited another target's streak")
	}
}

func TestApplyReachabilityThresholdsCoversWireGuardAndCloudflareChecks(t *testing.T) {
	uptimeHist.mu.Lock()
	previousEntryStreaks := uptimeHist.entryFailureStreaks
	previousWebStreaks := uptimeHist.webFailureStreaks
	previousEntryState := uptimeHist.entryReachableState
	previousWebState := uptimeHist.webReachableState
	uptimeHist.entryFailureStreaks = nil
	uptimeHist.webFailureStreaks = nil
	uptimeHist.entryReachableState = nil
	uptimeHist.webReachableState = nil
	uptimeHist.mu.Unlock()
	defer func() {
		uptimeHist.mu.Lock()
		uptimeHist.entryFailureStreaks = previousEntryStreaks
		uptimeHist.webFailureStreaks = previousWebStreaks
		uptimeHist.entryReachableState = previousEntryState
		uptimeHist.webReachableState = previousWebState
		uptimeHist.mu.Unlock()
	}()

	firstEntries := map[string]UptimeSample{"wireguard-entry": {Reachable: false}}
	firstWebRoutes := map[string]UptimeSample{"cloudflare-route": {Reachable: false}}
	cfg := AlertConfig{FailureThreshold: 3}
	applyReachabilityThresholdsLocked(firstEntries, firstWebRoutes, cfg)
	if !firstEntries["wireguard-entry"].Reachable {
		t.Fatal("first WireGuard failure marked the entry down")
	}
	if !firstWebRoutes["cloudflare-route"].Reachable {
		t.Fatal("first Cloudflare failure marked the route down")
	}

	secondEntries := map[string]UptimeSample{"wireguard-entry": {Reachable: false}}
	secondWebRoutes := map[string]UptimeSample{"cloudflare-route": {Reachable: false}}
	applyReachabilityThresholdsLocked(secondEntries, secondWebRoutes, cfg)
	if !secondEntries["wireguard-entry"].Reachable {
		t.Fatal("second WireGuard failure marked the entry down with a three-check threshold")
	}
	if !secondWebRoutes["cloudflare-route"].Reachable {
		t.Fatal("second Cloudflare failure marked the route down with a three-check threshold")
	}

	thirdEntries := map[string]UptimeSample{"wireguard-entry": {Reachable: false}}
	thirdWebRoutes := map[string]UptimeSample{"cloudflare-route": {Reachable: false}}
	applyReachabilityThresholdsLocked(thirdEntries, thirdWebRoutes, cfg)
	if thirdEntries["wireguard-entry"].Reachable {
		t.Fatal("third WireGuard failure did not mark the entry down")
	}
	if thirdWebRoutes["cloudflare-route"].Reachable {
		t.Fatal("third Cloudflare failure did not mark the route down")
	}
}

func TestIncreasingFailureThresholdDoesNotCreateFalseRecovery(t *testing.T) {
	uptimeHist.mu.Lock()
	previousEntryStreaks := uptimeHist.entryFailureStreaks
	previousWebStreaks := uptimeHist.webFailureStreaks
	previousEntryState := uptimeHist.entryReachableState
	previousWebState := uptimeHist.webReachableState
	uptimeHist.entryFailureStreaks = nil
	uptimeHist.webFailureStreaks = nil
	uptimeHist.entryReachableState = nil
	uptimeHist.webReachableState = nil
	uptimeHist.mu.Unlock()
	defer func() {
		uptimeHist.mu.Lock()
		uptimeHist.entryFailureStreaks = previousEntryStreaks
		uptimeHist.webFailureStreaks = previousWebStreaks
		uptimeHist.entryReachableState = previousEntryState
		uptimeHist.webReachableState = previousWebState
		uptimeHist.mu.Unlock()
	}()

	for i := 0; i < 2; i++ {
		entries := map[string]UptimeSample{"target": {Reachable: false}}
		applyReachabilityThresholdsLocked(entries, nil, AlertConfig{FailureThreshold: 2})
		if i == 1 && entries["target"].Reachable {
			t.Fatal("target did not transition down after two failures")
		}
	}

	stillFailing := map[string]UptimeSample{"target": {Reachable: false}}
	applyReachabilityThresholdsLocked(stillFailing, nil, AlertConfig{FailureThreshold: 5})
	if stillFailing["target"].Reachable {
		t.Fatal("raising the failure threshold manufactured a recovery without a successful probe")
	}

	recovered := map[string]UptimeSample{"target": {Reachable: true}}
	applyReachabilityThresholdsLocked(recovered, nil, AlertConfig{FailureThreshold: 5})
	if !recovered["target"].Reachable {
		t.Fatal("successful probe did not recover target after threshold increase")
	}
}

func TestDecreasingFailureThresholdAppliesOnNextFailedProbe(t *testing.T) {
	streaks := map[string]int{}
	states := map[string]bool{}
	for i := 0; i < 2; i++ {
		samples := map[string]UptimeSample{"target": {Reachable: false}}
		requireConsecutiveFailures(samples, streaks, states, 5)
		if !samples["target"].Reachable {
			t.Fatal("target went down before the original threshold")
		}
	}

	samples := map[string]UptimeSample{"target": {Reachable: false}}
	requireConsecutiveFailures(samples, streaks, states, 2)
	if samples["target"].Reachable {
		t.Fatal("lower threshold did not apply on the next failed probe")
	}
}

func TestHealthCheckDueUsesConfiguredInterval(t *testing.T) {
	now := time.Unix(1000, 0)
	cfg := AlertConfig{CheckIntervalSeconds: 90}
	if !healthCheckDue(time.Time{}, now, cfg) {
		t.Fatal("first health check was not due")
	}
	if healthCheckDue(now, now.Add(89*time.Second), cfg) {
		t.Fatal("health check ran before the configured 90-second interval")
	}
	if !healthCheckDue(now, now.Add(90*time.Second), cfg) {
		t.Fatal("health check was not due at the configured interval")
	}
}

func TestHealthCheckDueTracksTrafficIntervalIndependence(t *testing.T) {
	start := time.Unix(1000, 0)
	cfg := AlertConfig{CheckIntervalSeconds: 11}

	atTen := start.Add(10 * time.Second)
	if healthCheckDue(start, atTen, cfg) {
		t.Fatal("11-second health check ran at the 10-second traffic deadline")
	}

	atEleven := start.Add(11 * time.Second)
	if !healthCheckDue(start, atEleven, cfg) {
		t.Fatal("health check was not due at exactly 11 seconds")
	}
}

func TestSlowHealthSamplerDoesNotBlockTrafficSampler(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	healthTicks := make(chan time.Time)
	trafficTicks := make(chan time.Time)
	healthStarted := make(chan struct{})
	releaseHealth := make(chan struct{})
	trafficSamples := make(chan struct{}, 2)

	go runHealthSampler(ctx, healthTicks, func() AlertConfig {
		return AlertConfig{CheckIntervalSeconds: 10}
	}, func(context.Context) {
		close(healthStarted)
		<-releaseHealth
	})
	go runTrafficSampler(ctx, trafficTicks, func(context.Context) {
		trafficSamples <- struct{}{}
	})

	healthTicks <- time.Unix(1000, 0)
	select {
	case <-healthStarted:
	case <-time.After(time.Second):
		t.Fatal("slow health sample did not start")
	}

	trafficTicks <- time.Unix(1010, 0)
	trafficTicks <- time.Unix(1020, 0)
	for i := 0; i < 2; i++ {
		select {
		case <-trafficSamples:
		case <-time.After(time.Second):
			t.Fatal("traffic sampler was blocked by the in-flight health sample")
		}
	}
	close(releaseHealth)
}

func TestSamplerCancelsInFlightSampleOnShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan time.Time)
	started := make(chan context.Context, 1)
	finished := make(chan struct{})

	go runTrafficSampler(ctx, ticks, func(sampleCtx context.Context) {
		started <- sampleCtx
		<-finished
	})

	ticks <- time.Unix(1000, 0)
	var sampleCtx context.Context
	select {
	case sampleCtx = <-started:
	case <-time.After(time.Second):
		t.Fatal("sample did not start")
	}

	cancel()
	select {
	case <-sampleCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("in-flight sample context was not canceled on shutdown")
	}
	close(finished)
}

func TestSamplerRecoversFromPanicAndContinues(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan time.Time)
	samples := make(chan struct{}, 1)
	panicked := make(chan struct{})
	sample := func(context.Context) {
		select {
		case <-panicked:
			samples <- struct{}{}
		default:
			close(panicked)
			panic("boom")
		}
	}

	done := make(chan struct{})
	go func() {
		runTrafficSampler(ctx, ticks, sample)
		close(done)
	}()

	ticks <- time.Unix(1000, 0)
	select {
	case <-panicked:
	case <-done:
		t.Fatal("sampler stopped before its sample panic")
	case <-time.After(time.Second):
		t.Fatal("first sample did not begin")
	}

	ticks <- time.Unix(1010, 0)
	select {
	case <-samples:
	case <-done:
		t.Fatal("sampler loop stopped after a sample panic")
	case <-time.After(time.Second):
		t.Fatal("sampler did not recover after a sample panic")
	}
}

func TestUptimeMaxSamplesTracksConfiguredInterval(t *testing.T) {
	if got := uptimeMaxSamples(AlertConfig{RetentionDays: 1, CheckIntervalSeconds: 30}); got != 2880 {
		t.Fatalf("30-second one-day retention = %d samples, want 2880", got)
	}
	if got := uptimeMaxSamples(AlertConfig{RetentionDays: 1, CheckIntervalSeconds: 90}); got != 960 {
		t.Fatalf("90-second one-day retention = %d samples, want 960", got)
	}
	if got := uptimeMaxSamples(AlertConfig{RetentionDays: 365, CheckIntervalSeconds: 10}); got != MaxUptimeSamplesPerTarget {
		t.Fatalf("high-frequency retention = %d samples, want capped at %d", got, MaxUptimeSamplesPerTarget)
	}
}

func TestAppendUptimeSampleReusesExactCapacityRingWithoutFullCopy(t *testing.T) {
	history := map[string][]UptimeSample{}
	appendUptimeSample(history, "target", UptimeSample{LatencyMS: 1}, 3)
	appendUptimeSample(history, "target", UptimeSample{LatencyMS: 2}, 3)
	appendUptimeSample(history, "target", UptimeSample{LatencyMS: 3}, 3)

	appendUptimeSample(history, "target", UptimeSample{LatencyMS: 4}, 3)

	h := history["target"]
	if len(h) != 3 || cap(h) != 3 {
		t.Fatalf("ring len/cap = %d/%d, want 3/3", len(h), cap(h))
	}
	if h[0].LatencyMS != 2 || h[1].LatencyMS != 3 || h[2].LatencyMS != 4 {
		t.Fatalf("ring advanced incorrectly: %+v", h)
	}
}

func TestBoundedUptimeSamplesClampsAndCopiesOversizedHistory(t *testing.T) {
	samples := make([]UptimeSample, 8)
	for i := range samples {
		samples[i].LatencyMS = int64(i)
	}

	bounded := boundedUptimeSamples(samples, 3)
	if len(bounded) != 3 || cap(bounded) != 3 {
		t.Fatalf("bounded history len/cap = %d/%d, want 3/3", len(bounded), cap(bounded))
	}
	if bounded[0].LatencyMS != 5 || bounded[2].LatencyMS != 7 {
		t.Fatalf("bounded history kept the wrong samples: %+v", bounded)
	}

	samples[7].LatencyMS = 999
	if bounded[2].LatencyMS != 7 {
		t.Fatal("bounded history still aliases the oversized source allocation")
	}
}

func TestReboundUptimeHistoriesBoundsDisabledTargetsAfterPolicyChange(t *testing.T) {
	uptimeHist.mu.Lock()
	previousEntries := uptimeHist.entries
	previousWebRoutes := uptimeHist.webRoutes
	uptimeHist.entries = map[string][]UptimeSample{"disabled-entry": make([]UptimeSample, 8)}
	uptimeHist.webRoutes = map[string][]UptimeSample{"disabled-route": make([]UptimeSample, 7)}
	for i := range uptimeHist.entries["disabled-entry"] {
		uptimeHist.entries["disabled-entry"][i].LatencyMS = int64(i)
	}
	for i := range uptimeHist.webRoutes["disabled-route"] {
		uptimeHist.webRoutes["disabled-route"][i].LatencyMS = int64(i)
	}
	uptimeHist.mu.Unlock()
	defer func() {
		uptimeHist.mu.Lock()
		uptimeHist.entries = previousEntries
		uptimeHist.webRoutes = previousWebRoutes
		uptimeHist.mu.Unlock()
	}()

	reboundUptimeHistories(3)

	uptimeHist.mu.Lock()
	entry := uptimeHist.entries["disabled-entry"]
	route := uptimeHist.webRoutes["disabled-route"]
	uptimeHist.mu.Unlock()
	if len(entry) != 3 || cap(entry) != 3 || entry[0].LatencyMS != 5 {
		t.Fatalf("disabled entry was not rebound to its newest 3 samples: len/cap=%d/%d first=%d", len(entry), cap(entry), entry[0].LatencyMS)
	}
	if len(route) != 3 || cap(route) != 3 || route[0].LatencyMS != 4 {
		t.Fatalf("disabled route was not rebound to its newest 3 samples: len/cap=%d/%d first=%d", len(route), cap(route), route[0].LatencyMS)
	}
}

func TestLoadUptimeSnapshotBoundsDisabledHistoriesAndRestoresState(t *testing.T) {
	dir := t.TempDir()
	alerts, err := NewAlertConfigStore(filepath.Join(dir, "alerts.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := alerts.Set(AlertConfig{RetentionDays: 1, CheckIntervalSeconds: 3600}); err != nil {
		t.Fatal(err)
	}

	samples := make([]UptimeSample, 30)
	for i := range samples {
		samples[i] = UptimeSample{LatencyMS: int64(i), Reachable: i != 29}
	}
	blob, err := json.Marshal(uptimeSnapshot{Entries: map[string][]UptimeSample{"disabled-target": samples}})
	if err != nil {
		t.Fatal(err)
	}
	historyPath := filepath.Join(dir, "uptime-history.json")
	if err := os.WriteFile(historyPath, blob, 0o600); err != nil {
		t.Fatal(err)
	}

	uptimeHist.mu.Lock()
	previousEntries := uptimeHist.entries
	previousWebRoutes := uptimeHist.webRoutes
	previousEntryState := uptimeHist.entryReachableState
	previousWebState := uptimeHist.webReachableState
	uptimeHist.entries = nil
	uptimeHist.webRoutes = nil
	uptimeHist.entryReachableState = nil
	uptimeHist.webReachableState = nil
	uptimeHist.mu.Unlock()
	defer func() {
		uptimeHist.mu.Lock()
		uptimeHist.entries = previousEntries
		uptimeHist.webRoutes = previousWebRoutes
		uptimeHist.entryReachableState = previousEntryState
		uptimeHist.webReachableState = previousWebState
		uptimeHist.mu.Unlock()
	}()

	api := &API{alerts: alerts, historyPath: historyPath}
	api.loadUptimeSnapshot()

	uptimeHist.mu.Lock()
	loaded := uptimeHist.entries["disabled-target"]
	reachable := uptimeHist.entryReachableState["disabled-target"]
	uptimeHist.mu.Unlock()
	if len(loaded) != 24 || cap(loaded) != 24 {
		t.Fatalf("loaded history len/cap = %d/%d, want 24/24", len(loaded), cap(loaded))
	}
	if loaded[0].LatencyMS != 6 || loaded[23].LatencyMS != 29 {
		t.Fatalf("loaded history retained the wrong samples: first=%d last=%d", loaded[0].LatencyMS, loaded[23].LatencyMS)
	}
	if reachable {
		t.Fatal("loaded down state was not restored from the latest persisted sample")
	}
}
