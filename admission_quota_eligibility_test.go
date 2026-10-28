package main

import (
	"testing"
	"time"
)

func TestQuotaBlocksTrafficButNotGateProbe(t *testing.T) {
	a, clock, st := newAdmissionTest(t, 4, Candidate{Egress: "node:e1", Model: "m", Sub: "s"})
	now := clock.Now()
	key := quotaKey("s", "m")
	st.Quota[key] = &QuotaState{Until: now.Add(10 * time.Minute).UnixMilli()}

	if at, reason := a.AdmitPreferred("m", nil, ""); at != nil || reason != "quota" {
		t.Fatalf("traffic admitted under quota: at=%v reason=%q", at, reason)
	}

	delete(st.Quota, key)
	at, reason := a.AdmitPreferred("m", nil, "")
	if at == nil {
		t.Fatalf("traffic rejected after quota clear: %s", reason)
	}
	a.Observe(at, Outcome2xx)

	st.Quota[key] = &QuotaState{Until: now.Add(10 * time.Minute).UnixMilli()}
	st.Gate = &GateState{Active: true, CanaryOK: true, Egresses: map[string]int64{}}
	at, reason = a.AdmitGateProbe("m")
	if at == nil {
		t.Fatalf("gate probe rejected under quota: %s", reason)
	}
	a.Observe(at, Outcome2xx)
}

func TestEligibilityFiltersSeparateEntries(t *testing.T) {
	now := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	ms := now.UnixMilli()
	c := Candidate{Egress: "node:e1", Model: "m", Sub: "s"}

	t.Run("dead", func(t *testing.T) {
		a, _, st := newAdmissionTest(t, 4, c)
		st.Egress[c.Egress] = &EgressState{Lifecycle: "dead", DeadUntil: ms + 60000}
		if ok, _ := a.eligibleForTraffic(c, "m", ms); ok {
			t.Fatal("traffic allowed dead")
		}
		if ok, _ := a.eligibleForGateProbe(c, "m", ms); ok {
			t.Fatal("gate probe allowed dead")
		}
		if ok, _ := a.eligibleForManualProbe(c, "m", ms); !ok {
			t.Fatal("manual probe rejected dead")
		}
	})

	t.Run("pair", func(t *testing.T) {
		a, _, st := newAdmissionTest(t, 4, c)
		st.Pairs[PairKey(legacyPairModel, c.Egress)] = &PairState{Until: ms + 60000}
		if ok, _ := a.eligibleForTraffic(c, "m", ms); ok {
			t.Fatal("traffic allowed pair cooling")
		}
		if ok, _ := a.eligibleForGateProbe(c, "m", ms); !ok {
			t.Fatal("gate probe rejected pair cooling")
		}
		if ok, _ := a.eligibleForManualProbe(c, "m", ms); !ok {
			t.Fatal("manual probe rejected pair cooling")
		}
	})

	t.Run("quota", func(t *testing.T) {
		a, _, st := newAdmissionTest(t, 4, c)
		st.Quota[quotaKey("s", "m")] = &QuotaState{Until: ms + 60000}
		if ok, _ := a.eligibleForTraffic(c, "m", ms); ok {
			t.Fatal("traffic allowed quota")
		}
		if ok, _ := a.eligibleForGateProbe(c, "m", ms); !ok {
			t.Fatal("gate probe rejected quota")
		}
		if ok, _ := a.eligibleForManualProbe(c, "m", ms); !ok {
			t.Fatal("manual probe rejected quota")
		}
	})

	t.Run("sub", func(t *testing.T) {
		a, _, st := newAdmissionTest(t, 4, c)
		st.Subs[c.Sub] = &SubState{Until: ms + 60000}
		for name, fn := range map[string]func(Candidate, string, int64) (bool, bool){
			"traffic": a.eligibleForTraffic,
			"gate":    a.eligibleForGateProbe,
			"manual":  a.eligibleForManualProbe,
		} {
			ok, subBlocked := fn(c, "m", ms)
			if ok || !subBlocked {
				t.Fatalf("%s sub filter = (%v,%v), want false,true", name, ok, subBlocked)
			}
		}
	})
}
