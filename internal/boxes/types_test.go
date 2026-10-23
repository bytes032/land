package boxes

import (
	"bytes"
	"cmp"
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

// A claim of a type Hetzner refuses in the first location it tries, the cheapest it recommends, tries the type in the
// next cheapest location, by name, that sells it now, never one that doesn't, telling crabbox each location, and says
// where it goes.
func TestAClaimTriesItsTypeInEachLocationSellingIt(t *testing.T) {
	f := newFake(t)
	f.api.sell("cpx42", "fsn1", "hel1", "sin") // none in nbg1
	f.write(t, "sold-out", "cpx42 hel1\ncpx42 fsn1\n")
	p, _, stderr := testPool(t)
	b := claimed(t, p, "cpx42")
	if attempts := f.lines(t, "attempts", ""); !slices.Equal(attempts, []string{"cpx42 hel1", "cpx42 fsn1", "cpx42 sin"}) || b.c.Type != "cpx42" {
		t.Fatalf("crabbox tried %q and gave a %s; want a cpx42 in hel1, then fsn1, then sin", attempts, b.c.Type)
	}
	for _, said := range []string{"land: creating a cpx42 box in hel1\n", "land: no cpx42 available in hel1: creating one in fsn1 (€0.16/h)\n",
		"land: no cpx42 available in fsn1: creating one in sin (€0.21/h)\n"} {
		if !strings.Contains(stderr.String(), said) {
			t.Fatalf("the claim didn't say %q:\n%s", said, stderr)
		}
	}
}

// A claim of a type Hetzner sells nowhere now gets a new box of the cheapest bigger type, x86 and not deprecated, with
// as many cores and as much memory, whose disk holds the snapshot: never a cheaper one whose disk can't, nor an Arm one,
// a deprecated one or one sold nowhere either. It says what it creates instead, and the box is of that type.
func TestATypeSoldNowhereFallsBackToTheCheapestBiggerTypeTheSnapshotBoots(t *testing.T) {
	f := newFake(t)
	f.api.images[0].DiskSize = 320 // a cpx42's, more than cx43's
	f.api.sell("cpx42")
	p, _, stderr := testPool(t)
	b := claimed(t, p, "cpx42")
	if attempts := f.lines(t, "attempts", ""); !slices.Equal(attempts, []string{"cpx62 hel1"}) || b.c.Type != "cpx62" || p.record(t, b.c.Lease).Type != "cpx62" {
		t.Fatalf("crabbox tried %q and gave %+v; want a cpx62 in hel1, recorded so", attempts, b.c)
	}
	if images := f.lines(t, "images", ""); !slices.Equal(images, []string{"1"}) {
		t.Fatalf("the box started from %q, want the snapshot, image 1", images)
	}
	if said := "land: no cpx42 available: creating a cpx62 in hel1 (€0.29/h)\n"; !strings.Contains(stderr.String(), said) {
		t.Fatalf("the claim didn't say %q:\n%s", said, stderr)
	}
}

// A claim of a type whose disk is smaller than the snapshot's never asks Hetzner for one, which would refuse it in
// every location: it says why, and gets the cheapest bigger type whose disk holds the snapshot.
func TestATypeWhoseDiskCantBootTheSnapshotIsNeverTried(t *testing.T) {
	f := newFake(t)
	f.api.images[0].DiskSize = 320 // a cpx42's, more than cx43's
	p, _, stderr := testPool(t)
	b := claimed(t, p, "cx43")
	if attempts := f.lines(t, "attempts", ""); slices.ContainsFunc(attempts, func(a string) bool { return strings.HasPrefix(a, "cx43 ") }) ||
		b.c.Type == "cx43" {
		t.Fatalf("crabbox tried %q and gave a %s; want no cx43 tried", attempts, b.c.Type)
	}
	if said := "land: a cx43's 160 GB disk can't boot the snapshot, made on a 320 GB one\n"; !strings.Contains(stderr.String(), said) {
		t.Fatalf("the claim didn't say %q:\n%s", said, stderr)
	}
}

// A spare of a bigger type serves a claim only once the claim's own type can't be had: while Hetzner sells cpx42s, a
// claim of one creates one; once it sells none, the claim takes the spare cpx62, saying so, before any bigger new box,
// and the box it would have created goes.
func TestABiggerSpareServesOnlyOnceTheTypeCantBeHad(t *testing.T) {
	f := newFake(t)
	created := offTheHour()
	holder, _, _ := testPool(t)
	at(t, holder, created, created.Add(80*time.Minute))
	spare := claimed(t, holder, "cpx62")
	if err := holder.release(t.Context()); err != nil {
		t.Fatal(err)
	}
	creates, _, _ := testPool(t)
	at(t, creates, created, created.Add(90*time.Minute))
	if b := claimed(t, creates, "cpx42"); b.c.Lease == spare.c.Lease || b.c.Type != "cpx42" {
		t.Fatalf("with cpx42s for sale, a claim got %+v, not a new cpx42", b.c)
	}
	f.write(t, "sold-out", "cpx42\n")
	takes, _, stderr := testPool(t)
	at(t, takes, created, created.Add(90*time.Minute))
	before := len(f.lines(t, "attempts", ""))
	b := claimed(t, takes, "cpx42")
	if attempts := f.lines(t, "attempts", "")[before:]; b.c.Lease != spare.c.Lease ||
		!slices.Equal(attempts, []string{"cpx42 hel1", "cpx42 fsn1", "cpx42 nbg1", "cpx42 sin"}) {
		t.Fatalf("with no cpx42 to be had, crabbox tried %q and the claim got %+v; want each cpx42 tried, then the spare %s",
			attempts, b.c, spare.c.Lease)
	}
	if said := "land: taking spare " + spare.c.Lease + ", a cpx62, for a cpx42\n"; !strings.Contains(stderr.String(), said) {
		t.Fatalf("the claim didn't say %q:\n%s", said, stderr)
	}
	if held, folders := takes.claims(t), takes.folders(t); len(held) != 2 || held[takes.root] != spare.c.Lease || len(folders) != 2 {
		t.Fatalf("claims %v and folders %q; want the two boxes alone, the spare this worktree's", held, folders)
	}
}

// A claim of a type no box may be, one Hetzner doesn't have, an Arm one or a deprecated one, by --type or box.type, is
// refused, naming the type, why, and `land boxes types`, before any crabbox call, leaving no box folder.
func TestATypeNoBoxMayBeIsRefusedBeforeAnyCrabboxCall(t *testing.T) {
	f := newFake(t)
	p, _, _ := testPool(t)
	for _, c := range []struct{ typ, boxType, why string }{
		{"cpx99", "cpx42", "Hetzner has no such server type"},
		{"cax41", "cpx42", "its architecture is arm, and boxes are x86"},
		{"", "cpx51", "Hetzner has deprecated it"},
	} {
		p.cfg.Box.Type = c.boxType
		want := cmp.Or(c.typ, c.boxType)
		if _, err := p.claim(t.Context(), c.typ); err == nil || !strings.Contains(err.Error(), "no "+want+" box: "+c.why) ||
			!strings.Contains(err.Error(), "`land boxes types`") {
			t.Errorf("a claim of a %s: %v; want it refused, %s, pointing at land boxes types", want, err, c.why)
		}
	}
	if calls, folders := f.calls(t, ""), p.folders(t); len(calls) > 0 || len(folders) > 0 {
		t.Fatalf("crabbox calls %q, folders %q; want none", calls, folders)
	}
}

// A claim that Hetzner refuses every box it may get, of its type and bigger, says so once, with who holds the
// repository's boxes, then asks again with a new plan, of what Hetzner sells where by then, and gets its type there.
func TestAClaimRefusedEverywhereAsksAgainWithANewPlan(t *testing.T) {
	f := newFake(t)
	f.write(t, "sold-out", "cpx42\ncx43\ncpx62\nccx33\nccx43\nccx63\n")
	p, _, stderr := testPool(t)
	var slept []time.Duration
	var first []string
	p.sleep = func(_ context.Context, d time.Duration) error {
		slept, first = append(slept, d), f.lines(t, "attempts", "")
		f.write(t, "sold-out", "")
		f.api.sell("cpx42", "sin")
		return nil
	}
	b := claimed(t, p, "cpx42")
	// Each of the 4 locations of cpx42, then of cx43, cpx62, ccx33, ccx43 and ccx63.
	if again := f.lines(t, "attempts", "")[len(first):]; !slices.Equal(slept, []time.Duration{retryEvery}) || len(first) != 24 ||
		!slices.Equal(again, []string{"cpx42 sin"}) || b.c.Type != "cpx42" {
		t.Fatalf("slept %v after crabbox tried %q, then tried %q for a %s; want every box tried, one wait, then a cpx42 in sin, "+
			"where Hetzner sells one now", slept, first, again, b.c.Type)
	}
	if out := stderr.String(); strings.Count(out, "resource_unavailable") != 1 || strings.Count(out, "land: this repository's boxes: none\n") != 1 {
		t.Fatalf("want Hetzner's refusal and the holders said once:\n%s", out)
	}
}

// At the project's server limit, Hetzner refuses every new box, so a claim asks for no other, but a bigger spare still
// serves it; at the dedicated-core limit, the claim asks for no other dedicated box as big, refused the same, but still
// for shared ones.
func TestAtTheProjectsLimitsAClaimAsksOnlyForBoxesHetznerMayGive(t *testing.T) {
	f := newFake(t)
	created := offTheHour()
	holder, _, _ := testPool(t)
	at(t, holder, created, created.Add(80*time.Minute))
	spare := claimed(t, holder, "cpx62")
	if err := holder.release(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.write(t, "refuse", "1\n")
	full, _, _ := testPool(t)
	at(t, full, created, created.Add(90*time.Minute))
	before := len(f.lines(t, "attempts", ""))
	if b := claimed(t, full, "cpx42"); b.c.Lease != spare.c.Lease || !slices.Equal(f.lines(t, "attempts", "")[before:], []string{"cpx42 hel1"}) {
		t.Fatalf("at the server limit, crabbox tried %q and the claim got %+v; want one cpx42, then the spare %s",
			f.lines(t, "attempts", "")[before:], b.c, spare.c.Lease)
	}
	f.write(t, "dedicated-limit", "ccx33\nccx43\nccx63\n")
	f.write(t, "sold-out", "cpx62\n")
	dedicated, _, stderr := testPool(t)
	before = len(f.lines(t, "attempts", ""))
	if _, err := dedicated.claim(t.Context(), "ccx33"); err == nil || err.Error() != "slept" || !strings.Contains(stderr.String(), "dedicated_core_limit") {
		t.Fatalf("a claim Hetzner refused every box gave %v; want it to wait, its sleep failing, saying Hetzner's refusal:\n%s", err, stderr)
	}
	if attempts := f.lines(t, "attempts", "")[before:]; !slices.Equal(attempts, []string{"ccx33 hel1", "cpx62 hel1", "cpx62 fsn1", "cpx62 nbg1", "cpx62 sin"}) {
		t.Fatalf("at the dedicated-core limit, crabbox tried %q; want one ccx33, then the shared cpx62s alone", attempts)
	}
}

// A snapshot's build, whose snapshot's disk is its box's, never gets a bigger type than box.type: with none for sale,
// it waits, and builds on box.type once Hetzner sells one again.
func TestASnapshotIsBuiltOnlyOnBoxType(t *testing.T) {
	f := newFake(t)
	p, green := greenWorld(t, testConfig, "")
	f.write(t, "sold-out", "cpx42\n")
	var slept []time.Duration
	p.sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		f.write(t, "sold-out", "")
		return nil
	}
	if err := p.snapshot(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if attempts := f.lines(t, "attempts", ""); len(slept) != 1 ||
		!slices.Equal(attempts, []string{"cpx42 hel1", "cpx42 fsn1", "cpx42 nbg1", "cpx42 sin", "cpx42 hel1"}) {
		t.Fatalf("slept %v, crabbox tried %q; want each cpx42 refused, a wait, then a cpx42, never a bigger type", slept, attempts)
	}
	if _, images := f.api.changed(); len(images) != 1 || images[0].Labels["commit"] != green {
		t.Fatalf("snapshots %+v; want the new one, at %s", images, green)
	}
}

// `land boxes types` lists the types a box may be, Hetzner's x86 ones that aren't deprecated, cheapest first, by the
// price where Hetzner sells each now or, sold nowhere, its lowest, with where Hetzner sells each now.
func TestLandBoxesTypesListsTheTypesABoxMayBeCheapestFirst(t *testing.T) {
	f := newFake(t)
	f.api.sell("cpx42", "sin")
	var out bytes.Buffer
	if err := printTypes(t.Context(), hetzner{}, &out); err != nil {
		t.Fatal(err)
	}
	want := `TYPE   CPU        CORES  MEMORY  DISK    €/H     SOLD NOW IN
cx43   shared     8      16 GB   160 GB  0.0355  hel1 fsn1 nbg1 sin
cx53   shared     16     32 GB   320 GB  0.0673  none
cpx42  shared     8      16 GB   320 GB  0.2101  sin
cpx62  shared     16     32 GB   640 GB  0.2942  hel1 fsn1 nbg1 sin
ccx33  dedicated  8      32 GB   240 GB  0.3134  hel1 fsn1 nbg1 sin
ccx43  dedicated  16     64 GB   360 GB  0.6259  hel1 fsn1 nbg1 sin
ccx63  dedicated  48     192 GB  960 GB  1.9365  hel1 fsn1 nbg1 sin
`
	if out.String() != want {
		t.Fatalf("land boxes types printed\n%s\nwant\n%s", &out, want)
	}
}
