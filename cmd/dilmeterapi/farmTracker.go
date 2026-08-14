package main

import (
	"strconv"
	"sync"

	"github.com/Marcentus/Midir/packet"
)

// farmPropData is the payload of a "farm_prop" WebSocket message.
type farmPropData struct {
	Event        string `json:"event"` // "plant" | "tend" | "harvest"
	FieldProp    string `json:"fieldprop"`
	Owner        string `json:"owner"`
	ItemId       uint64 `json:"itemid"`
	Support      uint64 `json:"support"`
	SupportIndex uint64 `json:"supportIndex"`
	Fertility    bool   `json:"fertility"`
	Special      bool   `json:"special"`
}

type plotState struct {
	fieldprop    uint64
	owner        uint64
	itemid       uint64
	support      uint64
	supportIndex uint64
	fertility    bool
	special      bool
	plantEmitted bool
}

// FarmTracker correlates PropAppears/PropUpdate/PropDisappears packets into
// plant/tend/harvest events per plot (fieldprop).
//
// PropAppears never carries a fieldprop or itemid by itself - planting shows up
// as a short burst of PropUpdates (tags "seed"/"single") that cross-reference a
// seed-prop id and a field-prop id via linkprop/fieldprop before either carries
// itemid. FarmTracker resolves that cluster of ids to one fieldprop and only
// emits "plant" once an itemid is actually known for it.
type FarmTracker struct {
	mu sync.Mutex
	// idToField maps any prop id we've seen (appear id, linkprop id, the field's
	// own id) to the resolved fieldprop it belongs to.
	idToField map[uint64]uint64
	plots     map[uint64]*plotState // fieldprop -> state
}

func NewFarmTracker() *FarmTracker {
	return &FarmTracker{
		idToField: make(map[uint64]uint64),
		plots:     make(map[uint64]*plotState),
	}
}

func (f *FarmTracker) resolveField(candidates ...uint64) (uint64, bool) {
	for _, c := range candidates {
		if c == 0 {
			continue
		}
		if fp, ok := f.idToField[c]; ok {
			return fp, true
		}
	}
	return 0, false
}

// HandlePropAppear is currently a no-op: empirically, PropAppears' own Id is a
// generic/shared value (observed identical across unrelated plots and
// sessions), not a unique per-instance identifier, so it must never be used as
// an idToField correlation key - doing so would cross-contaminate unrelated
// plots. The real field<->seed correlation happens in HandlePropUpdate via the
// linkprop/fieldprop XML attributes, which are genuinely per-instance. Kept as
// a method (rather than removed) so the dispatch site stays symmetric with
// PropUpdate/PropDisappear and so future data revealing a use for LinkId here
// doesn't require re-plumbing the call site.
func (f *FarmTracker) HandlePropAppear(info *packet.PropAppearInfo) {
	_ = info
}

// HandlePropUpdate resolves/merges the fieldprop identity for this update,
// tracks plot state, and returns a farm_prop event to publish (or nil for
// ambient ticks / not-yet-fully-known plots).
func (f *FarmTracker) HandlePropUpdate(info *packet.PropUpdateInfo) *farmPropData {
	f.mu.Lock()
	defer f.mu.Unlock()

	var fieldprop uint64
	switch {
	case info.XML.HasFieldProp:
		fieldprop = info.XML.FieldProp
	default:
		if fp, ok := f.resolveField(info.Id, info.XML.LinkProp); ok {
			fieldprop = fp
		} else {
			// This packet's own id IS the field prop (e.g. the "single"-tagged
			// update at plant time, which has no fieldprop attribute yet since
			// it *is* the field record).
			fieldprop = info.Id
		}
	}

	f.idToField[info.Id] = fieldprop
	f.idToField[fieldprop] = fieldprop
	if info.XML.HasLinkProp {
		f.idToField[info.XML.LinkProp] = fieldprop
	}

	plot, known := f.plots[fieldprop]
	if !known {
		plot = &plotState{fieldprop: fieldprop}
		f.plots[fieldprop] = plot
	}

	if info.XML.Owner != 0 {
		plot.owner = info.XML.Owner
	}
	prevSupportIndex := plot.supportIndex

	if info.XML.HasItemId {
		plot.itemid = info.XML.ItemId
	}
	plot.support = info.XML.Support
	if info.XML.HasSupportIndex {
		plot.supportIndex = info.XML.SupportIndex
	}
	plot.fertility = info.XML.Fertility
	plot.special = info.XML.Special

	snapshot := func(event string) *farmPropData {
		return &farmPropData{
			Event:        event,
			FieldProp:    strconv.FormatUint(fieldprop, 10),
			Owner:        strconv.FormatUint(plot.owner, 10),
			ItemId:       plot.itemid,
			Support:      plot.support,
			SupportIndex: plot.supportIndex,
			Fertility:    plot.fertility,
			Special:      plot.special,
		}
	}

	// plant: first time we know both the fieldprop AND what was actually planted.
	if !plot.plantEmitted {
		if info.XML.HasItemId {
			plot.plantEmitted = true
			return snapshot("plant")
		}
		return nil
	}

	// tend: supportIndex genuinely reset to 0 (not just an ambient increment).
	if info.XML.HasSupportIndex && info.XML.SupportIndex == 0 && prevSupportIndex > 0 {
		return snapshot("tend")
	}

	// Ambient ticks (e.g. passive "grow" increments) - deliberately not emitted.
	return nil
}

// HandlePropDisappear resolves a disappearing prop's linked id (mirroring
// PropAppears' [id, linkId] shape - id is ignored here for the same reason
// it's ignored in HandlePropAppear: it's a generic shared value, not a unique
// per-instance identifier) back to a known fieldprop and, if found, emits
// harvest and forgets the plot.
func (f *FarmTracker) HandlePropDisappear(id, linkId uint64) *farmPropData {
	f.mu.Lock()
	defer f.mu.Unlock()

	fieldprop, ok := f.resolveField(linkId)
	if !ok {
		return nil
	}
	plot, known := f.plots[fieldprop]
	if !known {
		return nil
	}

	data := &farmPropData{
		Event:        "harvest",
		FieldProp:    strconv.FormatUint(fieldprop, 10),
		Owner:        strconv.FormatUint(plot.owner, 10),
		ItemId:       plot.itemid,
		Support:      plot.support,
		SupportIndex: plot.supportIndex,
		Fertility:    plot.fertility,
		Special:      plot.special,
	}

	delete(f.plots, fieldprop)
	for id, fp := range f.idToField {
		if fp == fieldprop {
			delete(f.idToField, id)
		}
	}

	return data
}
