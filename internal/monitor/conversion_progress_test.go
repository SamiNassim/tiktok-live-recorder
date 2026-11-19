package monitor

import (
	"testing"
	"time"

	"tiktok-live-recorder/internal/recording"
)

func TestConversionProgressSpansConversionAndVerification(t *testing.T) {
	p := conversionProgress{sourceDuration: 100 * time.Second}
	for _, step := range []struct {
		phase                  string
		seconds                int
		pass, percent, overall int
	}{
		{"Preparing conversion", 0, 0, -1, 0},
		{"Creating MP4", 0, 1, 0, 0},
		{"Creating MP4", 50, 1, 50, 25},
		{"Creating MP4", 100, 1, 99, 50},
		{"Verifying MP4", 0, 2, 0, 50},
		{"Verifying MP4", 50, 2, 50, 75},
		{"Verifying MP4", 100, 2, 99, 99},
		{"Saving MP4", 0, 2, -1, 99},
	} {
		pass, passes, percent, overall := p.update(recording.Progress{Phase: step.phase, MediaTime: time.Duration(step.seconds) * time.Second})
		if pass != step.pass || passes != 2 || percent != step.percent || overall != step.overall {
			t.Fatalf("%s at %ds: got pass %d/%d, %d%%, overall %d%%; want pass %d/2, %d%%, overall %d%%", step.phase, step.seconds, pass, passes, percent, overall, step.pass, step.percent, step.overall)
		}
	}
}

func TestConversionProgressAddsRecoveryPasses(t *testing.T) {
	for _, custom := range []bool{false, true} {
		p := conversionProgress{sourceDuration: 100 * time.Second, custom: custom}
		p.update(recording.Progress{Phase: "Creating MP4", MediaTime: 100 * time.Second})
		p.update(recording.Progress{Phase: "Verifying MP4", MediaTime: 100 * time.Second})
		pass, passes, percent, overall := p.update(recording.Progress{Phase: "Preparing recovery"})
		wantPasses, wantOverall := 4, 50
		if custom {
			wantPasses, wantOverall = 5, 40
		}
		if pass != 2 || passes != wantPasses || percent != -1 || overall != wantOverall {
			t.Fatalf("custom=%v: recovery lost completed work: %d/%d, %d%%, overall %d%%", custom, pass, passes, percent, overall)
		}
		p.update(recording.Progress{Phase: "Encoding MP4", MediaTime: 100 * time.Second})
		if custom {
			p.update(recording.Progress{Phase: "Applying options", MediaTime: 100 * time.Second})
		}
		pass, passes, percent, overall = p.update(recording.Progress{Phase: "Verifying MP4", MediaTime: 50 * time.Second})
		wantOverall = 87
		if custom {
			wantOverall = 90
		}
		if pass != wantPasses || passes != wantPasses || percent != 50 || overall != wantOverall {
			t.Fatalf("custom=%v: final verification: %d/%d, %d%%, overall %d%%", custom, pass, passes, percent, overall)
		}
	}
}

func TestConversionProgressUsesOutputDurationAndRejectsBackwardClocks(t *testing.T) {
	for _, estimate := range []time.Duration{0, 30 * time.Second} {
		p := conversionProgress{sourceDuration: estimate}
		_, _, percent, overall := p.update(recording.Progress{Phase: "Creating MP4", MediaTime: 10 * time.Second})
		if estimate == 0 && (percent != -1 || overall != -1) {
			t.Fatal("unknown duration produced a percentage", percent, overall)
		}
		p.update(recording.Progress{Phase: "Creating MP4", MediaTime: -time.Second})
		_, _, percent, overall = p.update(recording.Progress{Phase: "Verifying MP4"})
		if percent != 0 || overall != 50 {
			t.Fatal("verification did not use the successful output duration", percent, overall)
		}
		p.update(recording.Progress{Phase: "Verifying MP4", MediaTime: 5 * time.Second})
		_, _, percent, overall = p.update(recording.Progress{Phase: "Verifying MP4", MediaTime: 2 * time.Second})
		if percent != 50 || overall != 75 {
			t.Fatal("a backward clock lowered progress", percent, overall)
		}
	}
}

func TestConversionProgressCountsPartialFailedPass(t *testing.T) {
	p := conversionProgress{sourceDuration: 100 * time.Second}
	p.update(recording.Progress{Phase: "Creating MP4", MediaTime: 50 * time.Second})
	p.update(recording.Progress{Phase: "Preparing recovery"})
	pass, passes, percent, overall := p.update(recording.Progress{Phase: "Encoding MP4", MediaTime: 50 * time.Second})
	if pass != 2 || passes != 3 || percent != 50 || overall != 40 {
		t.Fatal("partial failed work was not counted", pass, passes, percent, overall)
	}
}

func TestConversionProgressAddsAutomaticRepairAfterFailedRecovery(t *testing.T) {
	p := conversionProgress{sourceDuration: 100 * time.Second, custom: true}
	p.update(recording.Progress{Phase: "Creating MP4", MediaTime: 100 * time.Second})
	p.update(recording.Progress{Phase: "Verifying MP4", MediaTime: 50 * time.Second})
	p.update(recording.Progress{Phase: "Preparing recovery"})
	p.update(recording.Progress{Phase: "Encoding MP4", MediaTime: 25 * time.Second})
	pass, passes, _, _ := p.update(recording.Progress{Phase: "Preparing repair"})
	if pass != 3 || passes != 5 {
		t.Fatal("automatic repair did not replace unused custom/recovery passes", pass, passes)
	}
	p.update(recording.Progress{Phase: "Repairing MP4", MediaTime: 100 * time.Second})
	pass, passes, percent, overall := p.update(recording.Progress{Phase: "Verifying MP4", MediaTime: 50 * time.Second})
	if pass != 5 || passes != 5 || percent != 50 || overall != 86 {
		t.Fatal("automatic repair lost prior work or final verification", pass, passes, percent, overall)
	}
}
