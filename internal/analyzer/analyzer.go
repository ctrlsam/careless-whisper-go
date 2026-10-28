package analyzer

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/ctrlsam/careless-whisper-go/internal/model"
)

const (
	screenOffRatio     = 1.6
	appBackgroundRatio = 1.25
	appStandbyRatio    = 2.1
	baselinePercentile = 0.2
	minScreenSamples   = 8
	minAppSamples      = 10
	recentSampleCount  = 10
)

const (
	// accessNetMinSamples is the number of receipts before an access-network
	// guess is attempted at all.
	accessNetMinSamples = 20
	// accessWindowSize is the receipt window used for the recent-state jitter
	// read and for each side of the handover comparison.
	accessWindowSize = 15
	// lanMaxMedianMs / lanMaxCV bound a wired companion: low, very stable RTT.
	lanMaxMedianMs = 120
	lanMaxCV       = 0.25
	// wifiMaxMedianMs / wifiMaxCV bound Wi-Fi: low-moderate RTT, low jitter.
	// Above these (higher jitter or denser timings) the guess falls to
	// cellular, matching the paper's qualitative LTE signature.
	wifiMaxMedianMs = 600
	wifiMaxCV       = 0.45
	// transitionRatio is the recent-vs-prior median RTT ratio that flags a
	// likely Wi-Fi/cellular handover in either direction.
	transitionRatio = 1.4
)

type Options struct {
	PingInterval  time.Duration
	ReferenceTime time.Time
}

func Analyze(capture model.Capture, options Options) model.Analysis {
	if options.PingInterval <= 0 {
		options.PingInterval = 2 * time.Second
	}
	if options.ReferenceTime.IsZero() {
		options.ReferenceTime = latestObservation(capture)
		if options.ReferenceTime.IsZero() {
			options.ReferenceTime = time.Now()
		}
	}

	structure := detectReceiptStructure(capture)
	deviceType := model.DeviceUnknown
	evidenceBatches := 0
	if structure != nil {
		evidenceBatches = structure.EvidenceBatches
		switch structure.Platform {
		case "iOS":
			deviceType = model.DeviceIPhone
		case "Android":
			deviceType = model.DeviceAndroid
		}
	}
	deviceConfidence := float64(evidenceBatches) / float64(evidenceBatches+1)

	delays := make([]float64, 0, len(capture.Reports))
	for _, report := range capture.Reports {
		delays = append(delays, report.DelayMillis())
	}

	analysis := model.Analysis{
		AnalyzedAt:       options.ReferenceTime,
		PhoneState:       model.ScreenOn,
		AppState:         model.AppStandby,
		DeviceType:       deviceType,
		OnlineStatus:     model.Offline,
		ReceiptStructure: structure,
		ConfidenceScores: map[string]float64{
			"overall":       0,
			"device_type":   deviceConfidence,
			"phone_state":   0,
			"app_state":     0,
			"online_status": 0,
		},
	}
	if len(delays) == 0 {
		return analysis
	}

	analysis.AverageRTTMillis = mean(delays)
	analysis.MedianRTTMillis = median(delays)
	analysis.StdDevRTTMillis = sampleStdDev(delays)
	analysis.TotalRequests = len(delays)
	analysis.PhoneState = phoneState(delays)
	analysis.AppState = appState(delays)
	analysis.OnlineStatus = onlineStatus(capture.Reports, options.ReferenceTime)
	analysis.CompanionDevices = companionDevices(capture.Reports)
	analysis.AccessNetwork = classifyAccessNetwork(delays, analysis.AppState, analysis.OnlineStatus)

	bytesPerMessage := 300
	if options.PingInterval < 100*time.Millisecond {
		bytesPerMessage = 500
		analysis.EstimatedBatteryDrainMin = 14
		analysis.EstimatedBatteryDrainMax = 18
	} else {
		if options.PingInterval < time.Second {
			bytesPerMessage = 400
		}
		analysis.EstimatedBatteryDrainMin = 1
		analysis.EstimatedBatteryDrainMax = 5
	}
	analysis.TotalDataUsedBytes = len(delays) * bytesPerMessage
	analysis.EstimatedDataRateBytesSec = float64(bytesPerMessage) / options.PingInterval.Seconds()

	sampleConfidence := math.Min(float64(len(delays))/50, 1)
	stabilityConfidence := clamp(1-analysis.StdDevRTTMillis/math.Max(analysis.AverageRTTMillis, 1), 0, 1)
	overall := round3(sampleConfidence * (0.7 + stabilityConfidence*0.3))
	analysis.ConfidenceScores["overall"] = overall
	analysis.ConfidenceScores["phone_state"] = overall
	analysis.ConfidenceScores["app_state"] = overall
	analysis.ConfidenceScores["online_status"] = math.Min(float64(len(delays))/5, 1)
	return analysis
}

func phoneState(delays []float64) model.PhoneState {
	if len(delays) < minScreenSamples {
		return model.ScreenOn
	}
	baseline := percentile(delays, baselinePercentile)
	if baseline <= 0 {
		return model.ScreenOn
	}
	if mean(tail(delays, recentSampleCount)) >= baseline*screenOffRatio {
		return model.ScreenOff
	}
	return model.ScreenOn
}

func appState(delays []float64) model.AppState {
	if len(delays) < minAppSamples {
		return model.AppForeground
	}
	baseline := percentile(delays, baselinePercentile)
	if baseline <= 0 {
		return model.AppForeground
	}
	ratio := mean(tail(delays, recentSampleCount)) / baseline
	switch {
	case ratio >= appStandbyRatio:
		return model.AppStandby
	case ratio >= appBackgroundRatio:
		return model.AppBackground
	default:
		return model.AppForeground
	}
}

func onlineStatus(reports []model.ReceiptReport, reference time.Time) model.OnlineStatus {
	for _, report := range reports {
		if !report.DeliveredAt.After(reference) && reference.Sub(report.DeliveredAt) < time.Minute {
			return model.Online
		}
	}
	return model.Offline
}

func detectReceiptStructure(capture model.Capture) *model.ReceiptStructure {
	var deliveries, reads []model.ReceiptEvent
	for _, event := range capture.Receipts {
		if event.DeviceID != 0 || !containsKnownMessage(event.MessageIDs, capture.SentMessages) {
			continue
		}
		switch strings.ToUpper(event.ReceiptType) {
		case "DELIVERED", "INACTIVE":
			deliveries = append(deliveries, event)
		case "READ":
			reads = append(reads, event)
		}
	}
	if len(deliveries) == 0 && len(reads) == 0 {
		return nil
	}

	deliveryHandling := "Unknown"
	if len(deliveries) > 0 {
		deliveryHandling = "Separate"
		for _, event := range deliveries {
			if len(event.MessageIDs) > 1 {
				deliveryHandling = "Stacked"
				break
			}
		}
	}

	readHandling := "Unknown"
	var stackedReads []model.ReceiptEvent
	if len(reads) > 0 {
		readHandling = "Separate"
		for _, event := range reads {
			if len(event.MessageIDs) > 1 {
				stackedReads = append(stackedReads, event)
			}
		}
		if len(stackedReads) > 0 {
			readHandling = "Stacked"
		}
	}

	orders := make(map[string]struct{})
	for _, event := range stackedReads {
		orders[receiptOrder(event, capture.SentMessages)] = struct{}{}
	}
	ordering := "Unknown"
	if len(orders) == 1 {
		for order := range orders {
			ordering = order
		}
	} else if len(orders) > 1 {
		ordering = "Mixed"
	}
	if len(stackedReads) > 0 && ordering != "Unknown" {
		readHandling = "Stacked (" + ordering + ")"
	}

	separateDeliveries := make(map[string]struct{})
	for _, event := range deliveries {
		if len(event.MessageIDs) == 1 {
			separateDeliveries[event.MessageIDs[0]] = struct{}{}
		}
	}
	completeBatches := make(map[string]struct{})
	for _, event := range stackedReads {
		order := receiptOrder(event, capture.SentMessages)
		if order != "Natural" && order != "Reversed" {
			continue
		}
		complete := true
		for _, id := range event.MessageIDs {
			if _, ok := separateDeliveries[id]; !ok {
				complete = false
				break
			}
		}
		if complete {
			completeBatches[strings.Join(event.MessageIDs, "\x00")] = struct{}{}
		}
	}

	platform := "Unknown"
	if deliveryHandling == "Separate" && len(completeBatches) > 0 {
		switch ordering {
		case "Reversed":
			platform = "iOS"
		case "Natural":
			platform = "Android"
		}
	}
	evidenceBatches := 0
	if platform != "Unknown" {
		evidenceBatches = len(completeBatches)
	}
	return &model.ReceiptStructure{
		DeliveryReceiptHandling: deliveryHandling,
		ReadReceiptHandling:     readHandling,
		ReceiptOrdering:         ordering,
		Platform:                platform,
		Messenger:               "WhatsApp",
		EvidenceBatches:         evidenceBatches,
	}
}

func receiptOrder(event model.ReceiptEvent, sent map[string]model.SentMessage) string {
	if len(event.MessageIDs) < 3 {
		return "Unknown"
	}
	seen := make(map[string]struct{}, len(event.MessageIDs))
	times := make([]time.Time, 0, len(event.MessageIDs))
	for _, id := range event.MessageIDs {
		message, ok := sent[id]
		if !ok {
			return "Unknown"
		}
		if _, duplicate := seen[id]; duplicate {
			return "Unknown"
		}
		seen[id] = struct{}{}
		times = append(times, message.SentAt)
	}
	for i := range times {
		for j := i + 1; j < len(times); j++ {
			if times[i].Equal(times[j]) {
				return "Unknown"
			}
		}
	}
	natural := slices.IsSortedFunc(times, func(a, b time.Time) int { return a.Compare(b) })
	reversed := slices.IsSortedFunc(times, func(a, b time.Time) int { return b.Compare(a) })
	if natural {
		return "Natural"
	}
	if reversed {
		return "Reversed"
	}
	return "Random"
}

func companionDevices(reports []model.ReceiptReport) []model.CompanionDevice {
	if len(reports) < 20 {
		return nil
	}
	type bucket struct {
		min, max                float64
		minCount                int
		kind, network, activity string
	}
	buckets := []bucket{
		{0, 100, 11, "Desktop/Web (low latency)", "LAN", "Active"},
		{100, 500, 11, "Mobile/Companion Device", "Wi-Fi/Cellular", "Active"},
		{500, 3000, 11, "Background/Standby Device", "Wi-Fi", "Inactive/Background"},
		{3000, math.Inf(1), 6, "Offline/Highly Delayed Device", "Unknown", "Offline"},
	}
	var result []model.CompanionDevice
	for _, candidate := range buckets {
		var matching []model.ReceiptReport
		for _, report := range reports {
			delay := report.DelayMillis()
			if delay >= candidate.min && delay < candidate.max {
				matching = append(matching, report)
			}
		}
		if len(matching) < candidate.minCount {
			continue
		}
		delays := make([]float64, len(matching))
		for i, report := range matching {
			delays[i] = report.DelayMillis()
		}
		average := mean(delays)
		result = append(result, model.CompanionDevice{
			DeviceIndex: len(result), Type: candidate.kind,
			AverageRTTMillis: average, StdDevRTTMillis: sampleStdDev(delays),
			Network: candidate.network, SampleCount: len(matching),
			Activity: candidate.activity, LastSeen: matching[len(matching)-1].DeliveredAt,
			Switching: detectSwitching(reports, average, 100),
		})
	}
	return result
}

func detectSwitching(reports []model.ReceiptReport, target, tolerance float64) bool {
	if len(reports) < 10 {
		return false
	}
	entries := 0
	inRange := false
	for _, report := range reports {
		current := math.Abs(report.DelayMillis()-target) <= tolerance
		if current && !inRange {
			entries++
		}
		inRange = current
	}
	return entries > 2
}

// classifyAccessNetwork makes a best-effort guess at the target's access
// network from the ordered delivery-receipt RTTs. Following the Careless
// Whisper paper, the signal is jitter (coefficient of variation) and a
// baseline step-change on handover, not absolute RTT. delays must be in
// delivery order. Returns nil until there are enough samples to say anything.
func classifyAccessNetwork(delays []float64, app model.AppState, online model.OnlineStatus) *model.AccessNetwork {
	if len(delays) < accessNetMinSamples {
		return nil
	}
	recent := tail(delays, accessWindowSize)
	recentMedian := median(recent)
	recentJitter := sampleStdDev(recent)
	cv := 0.0
	if recentMedian > 0 {
		cv = recentJitter / recentMedian
	}

	guess := &model.AccessNetwork{
		Type:              model.NetworkUndecided,
		RecentMedianRTTMs: round3(recentMedian),
		RecentJitterMs:    round3(recentJitter),
		CoeffVariation:    round3(cv),
	}

	// Handover detection: compare the window immediately before the recent one.
	// A sustained baseline step-change is the paper's strongest network signal
	// and holds even when the app is backgrounded, because it is relative.
	if len(delays) >= accessWindowSize*2 {
		prior := delays[len(delays)-accessWindowSize*2 : len(delays)-accessWindowSize]
		priorMedian := median(prior)
		if priorMedian > 0 {
			switch ratio := recentMedian / priorMedian; {
			case ratio >= transitionRatio:
				guess.TransitionDetected = true
				guess.TransitionNote = fmt.Sprintf(
					"RTT baseline rose %.0f -> %.0fms, possible Wi-Fi -> cellular handover", priorMedian, recentMedian)
			case ratio <= 1/transitionRatio:
				guess.TransitionDetected = true
				guess.TransitionNote = fmt.Sprintf(
					"RTT baseline fell %.0f -> %.0fms, possible cellular -> Wi-Fi handover", priorMedian, recentMedian)
			}
		}
	}

	// A static access-type guess only makes sense when the app is foreground
	// and online. A backgrounded or standby app's RTT is dominated by push-wake
	// latency, not the radio, so the type is left Unknown and only the handover
	// flag is trusted.
	if online == model.Online && app == model.AppForeground {
		switch {
		case recentMedian <= lanMaxMedianMs && cv <= lanMaxCV:
			guess.Type = model.NetworkWiredLAN
		case recentMedian <= wifiMaxMedianMs && cv <= wifiMaxCV:
			guess.Type = model.NetworkWiFi
		default:
			guess.Type = model.NetworkCellular
		}
	}

	// Confidence rises with sample count and a cleaner (lower-jitter) signal; a
	// flagged handover is the strongest single piece of evidence.
	sampleConfidence := math.Min(float64(len(delays))/50, 1)
	guess.Confidence = round3(sampleConfidence * clamp(1-cv, 0.2, 1))
	if guess.TransitionDetected {
		guess.Confidence = round3(math.Max(guess.Confidence, 0.6))
	}
	return guess
}

func latestObservation(capture model.Capture) time.Time {
	var latest time.Time
	for _, report := range capture.Reports {
		if report.DeliveredAt.After(latest) {
			latest = report.DeliveredAt
		}
	}
	for _, event := range capture.Receipts {
		if event.ReceivedAt.After(latest) {
			latest = event.ReceivedAt
		}
	}
	return latest
}

func containsKnownMessage(ids []string, sent map[string]model.SentMessage) bool {
	for _, id := range ids {
		if _, ok := sent[id]; ok {
			return true
		}
	}
	return false
}

func percentile(values []float64, pct float64) float64 {
	if len(values) == 0 {
		return 0
	}
	ordered := slices.Clone(values)
	slices.Sort(ordered)
	if len(ordered) == 1 {
		return ordered[0]
	}
	position := float64(len(ordered)-1) * pct
	lower := int(position)
	upper := min(lower+1, len(ordered)-1)
	return ordered[lower] + (ordered[upper]-ordered[lower])*(position-float64(lower))
}

func mean(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	var sum float64
	for _, value := range values {
		sum += value
	}
	return sum / float64(len(values))
}

func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	ordered := slices.Clone(values)
	slices.Sort(ordered)
	middle := len(ordered) / 2
	if len(ordered)%2 == 1 {
		return ordered[middle]
	}
	return (ordered[middle-1] + ordered[middle]) / 2
}

func sampleStdDev(values []float64) float64 {
	if len(values) < 2 {
		return 0
	}
	average := mean(values)
	var squared float64
	for _, value := range values {
		delta := value - average
		squared += delta * delta
	}
	return math.Sqrt(squared / float64(len(values)-1))
}

func tail(values []float64, count int) []float64 {
	if len(values) <= count {
		return values
	}
	return values[len(values)-count:]
}

func clamp(value, low, high float64) float64 {
	return math.Max(low, math.Min(high, value))
}

func round3(value float64) float64 {
	return math.Round(value*1000) / 1000
}
