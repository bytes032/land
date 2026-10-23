package boxes

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
)

// askable is the server type typ among types, as a claim may ask for it: one Hetzner has, x86, as crabbox's images and
// so every snapshot are, and not deprecated. land creates a box of no other.
func askable(types []serverType, typ string) (serverType, error) {
	t, ok := typeNamed(types, typ)
	why := ""
	switch {
	case !ok:
		why = "Hetzner has no such server type"
	case t.Architecture != "x86":
		why = "its architecture is " + t.Architecture + ", and boxes are x86"
	case t.Deprecation != nil:
		why = "Hetzner has deprecated it"
	default:
		return t, nil
	}
	return serverType{}, fmt.Errorf("no %s box: %s; `land boxes types` lists the types a box may be", typ, why)
}

func typeNamed(types []serverType, name string) (serverType, bool) {
	i := slices.IndexFunc(types, func(t serverType) bool { return t.Name == name })
	if i < 0 {
		return serverType{}, false
	}
	return types[i], true
}

// atLeast reports whether a box of type t serves a claim asking for asked: x86, with as many cores and as much memory.
func atLeast(t, asked serverType) bool {
	return t.Architecture == "x86" && t.Cores >= asked.Cores && t.Memory >= asked.Memory
}

// bigger are the types a new box may be when Hetzner sells none of asked: x86, not deprecated, at least as big, and
// with a disk of at least disk GB, the snapshot's, which boots on no smaller one.
func bigger(types []serverType, asked serverType, disk float64) []serverType {
	return slices.DeleteFunc(slices.Clone(types), func(t serverType) bool {
		return t.Name == asked.Name || t.Deprecation != nil || !atLeast(t, asked) || t.Disk < disk
	})
}

// offer is a server type Hetzner sells now in a location, at its hourly price there.
type offer struct {
	typ         serverType
	location    string
	price       float64
	recommended bool
}

// offers are where Hetzner sells each of types now, cheapest first, then in a location it recommends, then by name.
func offers(types ...serverType) []offer {
	var all []offer
	for _, t := range types {
		for _, l := range t.Locations {
			if price, ok := hourly(t, l.Name); ok && l.Available {
				all = append(all, offer{t, l.Name, price, l.Recommended})
			}
		}
	}
	unrecommended := func(o offer) int {
		if o.recommended {
			return 0
		}
		return 1
	}
	slices.SortFunc(all, func(a, b offer) int {
		return cmp.Or(cmp.Compare(a.price, b.price), cmp.Compare(unrecommended(a), unrecommended(b)),
			strings.Compare(a.typ.Name, b.typ.Name), strings.Compare(a.location, b.location))
	})
	return all
}

// hourly is the type's hourly price in location, when Hetzner prices it there.
func hourly(t serverType, location string) (float64, bool) {
	i := slices.IndexFunc(t.Prices, func(p typePrice) bool { return p.Location == location })
	if i < 0 {
		return 0, false
	}
	price, err := strconv.ParseFloat(t.Prices[i].Hourly.Gross, 64)
	return price, err == nil
}

// printTypes is `land boxes types`: the types a box may be, Hetzner's x86 ones it hasn't deprecated, each with its CPU,
// cores, memory and disk, its hourly price where Hetzner sells it now most cheaply, or, sold nowhere, where it prices
// it lowest, and the locations selling it now, cheapest first.
func printTypes(ctx context.Context, hz hetzner, out io.Writer) error {
	types, err := hz.serverTypes(ctx)
	if err != nil {
		return err
	}
	type row struct {
		t     serverType
		price float64
		in    []string
	}
	var rows []row
	for _, t := range types {
		if t.Architecture != "x86" || t.Deprecation != nil {
			continue
		}
		r := row{t: t, price: math.Inf(1)}
		for _, o := range offers(t) {
			r.in = append(r.in, o.location)
			r.price = min(r.price, o.price)
		}
		for _, p := range t.Prices {
			if price, ok := hourly(t, p.Location); ok && len(r.in) == 0 {
				r.price = min(r.price, price)
			}
		}
		rows = append(rows, r)
	}
	slices.SortFunc(rows, func(a, b row) int { return cmp.Or(cmp.Compare(a.price, b.price), strings.Compare(a.t.Name, b.t.Name)) })
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TYPE\tCPU\tCORES\tMEMORY\tDISK\t€/H\tSOLD NOW IN")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%d\t%g GB\t%g GB\t%.4f\t%s\n", r.t.Name, r.t.CPUType, r.t.Cores, r.t.Memory, r.t.Disk, r.price,
			cmp.Or(strings.Join(r.in, " "), "none"))
	}
	return w.Flush()
}
