package web

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"xm/internal/archive"
)

func TestSchedulerAutoIdentityRecovery(t *testing.T) {
	for _, scenario := range []string{"renamed", "same", "mismatch", "auth", "rate", "suspended", "unresolved", "unbound", "cancelled", "repeat_missing", "partial", "explicit_suspended", "manual_paused", "username_missing"} {
		t.Run(scenario, func(t *testing.T) {
			a := testApp(t)
			seedMonitor(t, a, "alice")
			if scenario != "unbound" {
				if err := a.store.SaveTargetProfile("alice", archive.Profile{ID: "42", Username: "alice"}); err != nil {
					t.Fatal(err)
				}
				if err := a.store.SavePosts("alice", []archive.Post{{ID: "123", AuthorID: "42", Username: "alice"}}); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "manual_paused" {
				if _, err := a.store.DB.Exec("UPDATE monitors SET enabled=0 WHERE username='alice'"); err != nil {
					t.Fatal(err)
				}
				if err := a.scheduler.queue("alice"); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			lookupCalls, collectCalls := 0, 0
			a.refreshIdentity = func(_ context.Context, _ archive.Config, _ archive.Cookies, i archive.TargetIdentity, refs []string, name string) (archive.Profile, error) {
				lookupCalls++
				if i.UserID != "42" || len(refs) != 1 || refs[0] != "123" || name != "" {
					t.Fatal(i, refs, name)
				}
				if a.reserveIdentityRefresh() {
					t.Fatal("automatic lookup released serial reservation")
				}
				switch scenario {
				case "same":
					return archive.Profile{ID: "42", Username: "alice"}, nil
				case "mismatch":
					return archive.Profile{ID: "99", Username: "newalice"}, nil
				case "auth", "rate":
					return archive.Profile{}, &archive.Failure{Kind: scenario, Message: scenario + " stop"}
				case "suspended":
					return archive.Profile{}, &archive.Failure{Kind: "access", Reason: "suspended", Message: archive.AccountSuspendedMessage}
				case "unresolved":
					return archive.Profile{}, errors.New("lookup failed")
				case "cancelled":
					cancel()
				}
				return archive.Profile{ID: "42", Username: "newalice"}, nil
			}
			a.scheduler.collect = func(_ context.Context, _ archive.Config, _ archive.Cookies, target string, i archive.TargetIdentity, _ int, _ bool, save func(archive.Page) error) (archive.Collection, error) {
				collectCalls++
				if target != "alice" {
					t.Fatal("archive key changed", target)
				}
				if collectCalls == 1 {
					if scenario == "partial" {
						if err := save(archive.Page{Posts: []archive.Post{{ID: "124", AuthorID: "42", Username: "alice"}}}); err != nil {
							t.Fatal(err)
						}
					}
					if scenario == "explicit_suspended" {
						return archive.Collection{}, &archive.Failure{Kind: "access", Reason: "suspended", Message: archive.AccountSuspendedMessage}
					}
					if scenario == "username_missing" {
						return archive.Collection{}, &archive.Failure{Kind: "access", Reason: "username_missing", Message: archive.AccountLookupMissingMessage}
					}
					return archive.Collection{}, &archive.Failure{Kind: "access", Reason: "not_found", Message: archive.AccountNotFoundMessage}
				}
				if collectCalls > 2 || i.UserID != "42" || i.CurrentUsername != "newalice" {
					t.Fatal(collectCalls, i)
				}
				if scenario == "repeat_missing" {
					return archive.Collection{}, &archive.Failure{Kind: "access", Reason: "not_found", Message: archive.AccountNotFoundMessage}
				}
				return archive.Collection{Stop: "known_boundary_reached"}, nil
			}
			worked, err := a.scheduler.step(ctx)
			if !worked || err != nil {
				t.Fatal(worked, err)
			}
			wantLookups := 1
			if scenario == "unbound" || scenario == "partial" || scenario == "explicit_suspended" {
				wantLookups = 0
			}
			wantCollect := 1
			if scenario == "renamed" || scenario == "repeat_missing" || scenario == "manual_paused" || scenario == "username_missing" {
				wantCollect = 2
			}
			if lookupCalls != wantLookups || collectCalls != wantCollect {
				t.Fatal(lookupCalls, collectCalls)
			}
			ms, err := monitors(a.store.DB)
			if err != nil || len(ms) != 1 {
				t.Fatal(ms, err)
			}
			m := ms[0]
			i, err := a.store.TargetIdentity("alice")
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "renamed" || scenario == "repeat_missing" || scenario == "manual_paused" || scenario == "username_missing" {
				if i.CurrentUsername != "newalice" || i.UserID != "42" {
					t.Fatal(i)
				}
			} else if i.CurrentUsername != "alice" {
				t.Fatal("identity changed without confirmation", i)
			}
			if scenario == "renamed" || scenario == "username_missing" {
				if !m.Enabled || m.LastError != "" || m.Attention != nil {
					t.Fatal(m)
				}
			}
			if scenario == "same" || scenario == "mismatch" || scenario == "unresolved" || scenario == "unbound" || scenario == "repeat_missing" || scenario == "explicit_suspended" || scenario == "suspended" {
				if m.Enabled || m.Attention == nil {
					t.Fatal(m)
				}
			}
			if scenario == "suspended" || scenario == "explicit_suspended" {
				if m.Attention.Code != "suspended" {
					t.Fatal(m.Attention)
				}
			}
			if scenario == "manual_paused" && (m.Enabled || m.LastError != "") {
				t.Fatal("manual collection changed pause", m)
			}
			gates, err := loadSchedulerState(a.store.DB)
			if err != nil {
				t.Fatal(err)
			}
			if (gates.CookieBlocked != "") != (scenario == "auth") {
				t.Fatal(gates)
			}
			if scenario == "rate" && gates.CooldownUntil <= time.Now().Unix() {
				t.Fatal(gates)
			}
			if scenario == "unresolved" && !strings.Contains(m.LastError, "自动更新账号身份未成功") {
				t.Fatal(m.LastError)
			}
		})
	}
}
