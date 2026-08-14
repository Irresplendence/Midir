package main

import (
	"strconv"
	"sync"
	"time"

	"github.com/Marcentus/Midir/packet"
)

// farmPropData is the payload of a "farm_prop" WebSocket message.
type farmPropData struct {
	Event        string `json:"event"` // "plant" | "tend" | "ready" | "harvest"
	FieldProp    string `json:"fieldprop"`
	Owner        string `json:"owner"`
	ItemId       uint64 `json:"itemid"`
	Name         string `json:"name,omitempty"` // item display name, e.g. "Blackberry Seeds"; empty if not yet correlated
	Support      uint64 `json:"support"`
	SupportIndex uint64 `json:"supportIndex"`
	Fertility    bool   `json:"fertility"`
	Special      bool   `json:"special"`
	At           int64  `json:"at"` // unix seconds, packet arrival time
}

type plotState struct {
	fieldprop      uint64
	owner          uint64
	itemid         uint64
	name           string
	support        uint64
	supportIndex   uint64
	fertility      bool
	special        bool
	plantEmitted   bool
	readyEmitted   bool
	harvestEmitted bool
}

// FarmTracker correlates PropAppears/PropUpdate/PropDisappears packets into
// plant/tend/ready/harvest events per plot (fieldprop).
//
// Two different kinds of prop have been observed:
//
//   - Crop plots (tags "seed"/"single"/"grow"): PropAppears never carries a
//     fieldprop or itemid by itself - planting shows up as a short burst of
//     PropUpdates that cross-reference a seed-prop id and a field-prop id via
//     linkprop/fieldprop before either carries itemid. They fully disappear
//     (PropDisappears) on harvest, since the planted crop is a temporary
//     player-owned object.
//   - Renewable nodes - Tree/Quartz/Spider (tags "seed"/"empty"/"collecting"):
//     a single PropUpdate at plant time already carries fieldprop==linkprop==id
//     plus itemid, no multi-packet correlation needed. They never disappear
//     (persistent, regrowable world objects) - harvest is signaled by the
//     "collecting" tag instead of PropDisappears.
//
// FarmTracker resolves either shape to one fieldprop and only emits "plant"
// once an itemid is actually known for it, regardless of which tag vocabulary
// produced it.
type FarmTracker struct {
	mu sync.Mutex
	// idToField maps any prop id we've seen (appear id, linkprop id, the field's
	// own id) to the resolved fieldprop it belongs to.
	idToField map[uint64]uint64
	plots     map[uint64]*plotState // fieldprop -> state

	// pendingPlantName/pendingPlantAt cache the most recent "You used <item>
	// (<farm>)!" system message (opcode 0x526d), applied to the next "plant"
	// event if it fires within plantMessageCorrelationWindow. There's no shared
	// ID linking that message to a specific fieldprop, so this is a simple
	// single-slot cache rather than per-plot matching - safe in practice since
	// one player only plants one thing at a time.
	pendingPlantName string
	pendingPlantAt   time.Time
}

// plantMessageCorrelationWindow bounds how long a cached plant-confirmation
// message stays eligible to be attached to the next plant event. Crop
// planting takes several packets (and observed up to a few seconds) to
// resolve fieldprop+itemid, so this needs to be generous, not tight.
const plantMessageCorrelationWindow = 15 * time.Second

func NewFarmTracker() *FarmTracker {
	return &FarmTracker{
		idToField: make(map[uint64]uint64),
		plots:     make(map[uint64]*plotState),
	}
}

// HandlePlantMessage caches a plant-confirmation item name (already extracted
// via packet.ParseFarmSystemMessage) to be attached to the next plant event.
func (f *FarmTracker) HandlePlantMessage(itemName string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pendingPlantName = itemName
	f.pendingPlantAt = at
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
			Name:         plot.name,
			Support:      plot.support,
			SupportIndex: plot.supportIndex,
			Fertility:    plot.fertility,
			Special:      plot.special,
			At:           info.At.Unix(),
		}
	}

	// plant: first time we know both the fieldprop AND what was actually planted.
	if !plot.plantEmitted {
		if info.XML.HasItemId {
			plot.plantEmitted = true
			if f.pendingPlantName != "" && !info.At.IsZero() {
				delta := info.At.Sub(f.pendingPlantAt)
				if delta >= -2*time.Second && delta < plantMessageCorrelationWindow {
					plot.name = f.pendingPlantName
					f.pendingPlantName = ""
				}
			}
			return snapshot("plant")
		}
		return nil
	}

	// ready: the "completed" tag - confirmed via a live timed capture to fire
	// exactly when the crop finishes growing (packet timestamp landed ~12m02s
	// after the plant event, matching the in-game countdown almost exactly).
	// Guarded by readyEmitted since the game keeps re-sending "completed" on
	// ambient ticks after the crop is ready, same as it does for "grow".
	if info.Tag == "completed" && !plot.readyEmitted {
		plot.readyEmitted = true
		return snapshot("ready")
	}

	// harvest (renewable nodes only): Tree/Quartz/Spider never fire
	// PropDisappears, so "collecting" - which fires as the harvest action
	// itself - is the only harvest signal available for them. Checked before
	// tend so the two can't both fire off the same packet.
	if info.Tag == "collecting" && !plot.harvestEmitted {
		plot.harvestEmitted = true
		data := snapshot("harvest")
		f.resetPlotForNextCycle(plot)
		return data
	}

	// tend: supportIndex genuinely reset to 0 (not just an ambient increment).
	if info.XML.HasSupportIndex && info.XML.SupportIndex == 0 && prevSupportIndex > 0 {
		return snapshot("tend")
	}

	// Ambient ticks (e.g. passive "grow"/"completed" repeats) - deliberately not emitted.
	return nil
}

// resetPlotForNextCycle only clears harvestEmitted after a renewable node's
// "collecting"-triggered harvest - it deliberately leaves plantEmitted and
// readyEmitted set. A real capture showed the harvest transaction's own
// trailing "empty" state-echo packets still carrying the old itemid (and a
// non-fresh, non-zero supportIndex) immediately after "collecting" fires;
// resetting plantEmitted here caused those echoes to be misread as a fresh
// plant. The tradeoff: if the same fieldprop is later genuinely replanted
// (unconfirmed whether these nodes even reuse a fieldprop across regrowth
// cycles, as opposed to getting a fresh one like crops do), that replant's
// plant/ready won't re-fire. Missing a real replant is a safer failure mode
// than fabricating a plant event that never happened.
func (f *FarmTracker) resetPlotForNextCycle(plot *plotState) {
	plot.harvestEmitted = false
}

// HandlePropDisappear resolves a disappearing prop's linked id (mirroring
// PropAppears' [id, linkId] shape - id is ignored here for the same reason
// it's ignored in HandlePropAppear: it's a generic shared value, not a unique
// per-instance identifier) back to a known fieldprop and, if found, emits
// harvest and forgets the plot.
func (f *FarmTracker) HandlePropDisappear(id, linkId uint64, at time.Time) *farmPropData {
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
		Name:         plot.name,
		Support:      plot.support,
		SupportIndex: plot.supportIndex,
		Fertility:    plot.fertility,
		Special:      plot.special,
		At:           at.Unix(),
	}

	delete(f.plots, fieldprop)
	for id, fp := range f.idToField {
		if fp == fieldprop {
			delete(f.idToField, id)
		}
	}

	return data
}
