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
	Quality      string `json:"quality,omitempty"` // "Common" | "Fine" | "Finest" - only ever set on "harvest" events
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
	harvestedAt    time.Time // zero until this plot's first renewable-node harvest; see renewableReplantDebounce
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

	// pendingHarvestQuality caches the most recent harvest-confirmation message
	// (opcode 0x213a6, "<Quality> <item> ... placed in storage."), applied to
	// the next harvest event the same way pendingPlantName is applied to the
	// next plant event. This is the only place quality (Common/Fine/Finest)
	// appears on the wire - there's no separate numeric score field.
	pendingHarvestQuality string
	pendingHarvestAt      time.Time

	// pendingHarvestEmit holds a harvest event that's ready except for
	// quality, for the renewable-node ordering (see HandleHarvestMessage)
	// where the harvest trigger fires before the quality message does.
	pendingHarvestEmit *farmPropData
}

// plantMessageCorrelationWindow bounds how long a cached plant-confirmation
// message stays eligible to be attached to the next plant event. Crop
// planting takes several packets (and observed up to a few seconds) to
// resolve fieldprop+itemid, so this needs to be generous, not tight.
const plantMessageCorrelationWindow = 15 * time.Second

// harvestMessageCorrelationWindow bounds how long a cached harvest-storage
// message stays eligible to be attached to the next harvest event. Observed
// arriving in the same packet burst as the harvest signal itself, but kept
// generous for the same reason as plantMessageCorrelationWindow.
const harvestMessageCorrelationWindow = 15 * time.Second

// renewableReplantDebounce bounds how soon after a renewable node's harvest a
// plant-shaped update can be treated as a genuine replant rather than the
// harvest transaction's own trailing state-echo packets (which arrive within
// the same burst as the harvest, still carrying the old itemid - see
// resetPlotForNextCycle). A real replant requires the player to physically
// walk over and interact again, which takes several seconds at minimum, so a
// short debounce filters the echo out without adding noticeable latency to a
// genuine replant.
const renewableReplantDebounce = 2 * time.Second

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

// HandleHarvestMessage handles a harvest-confirmation quality (already
// extracted via packet.ParseHarvestStorageMessage). Two different orderings
// have been observed: crops fire this message *before* the harvest trigger
// (PropDisappears), so there's nothing pending yet and it's just cached for
// HandlePropDisappear to pick up. Renewable nodes fire "collecting" *before*
// this message, so HandlePropUpdate will already have parked a
// quality-less harvest event in pendingHarvestEmit - if so, this completes
// and returns it for the caller to actually publish (the only case where
// completing a harvest happens outside HandlePropUpdate/HandlePropDisappear).
func (f *FarmTracker) HandleHarvestMessage(quality string, at time.Time) *farmPropData {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.pendingHarvestEmit != nil {
		data := f.pendingHarvestEmit
		f.pendingHarvestEmit = nil
		data.Quality = quality
		return data
	}

	f.pendingHarvestQuality = quality
	f.pendingHarvestAt = at
	return nil
}

// takeHarvestQuality returns the pending harvest quality if it's still within
// the correlation window of `at`, consuming it (single-use) either way once
// checked so a stale one can't leak into some unrelated future harvest.
func (f *FarmTracker) takeHarvestQuality(at time.Time) string {
	quality := f.pendingHarvestQuality
	f.pendingHarvestQuality = ""
	if quality == "" || at.IsZero() {
		return ""
	}
	delta := at.Sub(f.pendingHarvestAt)
	if delta < -2*time.Second || delta >= harvestMessageCorrelationWindow {
		return ""
	}
	return quality
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
	// fieldprop="0" is a real, observed value on renewable nodes' post-harvest
	// reset packets (a "cleared" sentinel, not an actual plot id - confirmed
	// via a live capture: a Quartz harvest's second reset packet had
	// fieldprop="0" while still carrying the real itemid). Treating it as a
	// literal id created a phantom fieldprop-"0" plot and fired a spurious
	// "plant" for it. Falling through to resolution by the packet's own Id
	// instead correctly maps it back to the real plot, since Id stays
	// constant and already has a idToField entry from the packet(s) before it.
	case info.XML.HasFieldProp && info.XML.FieldProp != 0:
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

	// harvest (renewable nodes only): Tree/Quartz/Spider never fire
	// PropDisappears, so the signal is linkprop clearing to 0 instead -
	// confirmed as the universal renewable-node harvest signal across all
	// three: a live Quartz and Tree capture showed the post-harvest packet
	// going straight to linkprop="0" with no "collecting" tag involved at
	// all (that was a Spider-only extra step, wrongly over-generalized in an
	// earlier version of this code - Spider does also clear linkprop=0 a
	// couple packets after its "collecting", so keying off this instead
	// covers all three node types with one condition).
	//
	// tag != "single" scopes this to renewable nodes only: crops show this
	// exact same linkprop="0" pattern too (on their "single"-tagged field
	// record, right before PropDisappears), which is already handled by
	// HandlePropDisappear - without this exclusion, crop harvests would
	// double-fire, once here and once there. Renewable nodes never use
	// "single" for anything (their one per-plot record is always tagged
	// "seed" or "empty"), so this doesn't cost them any coverage.
	if info.XML.HasLinkProp && info.XML.LinkProp == 0 && info.Tag != "single" && !plot.harvestEmitted {
		plot.harvestEmitted = true
		data := snapshot("harvest")
		f.resetPlotForNextCycle(plot, info.At)
		if q := f.takeHarvestQuality(info.At); q != "" {
			data.Quality = q
			return data
		}
		// The harvest confirmation message has arrived before this trigger
		// in every renewable-node capture seen so far, so this path (park
		// and wait) is expected to be a rare fallback rather than the norm -
		// kept anyway since it's a strict superset of "just check the cache"
		// and costs nothing to leave in place.
		f.pendingHarvestEmit = data
		return nil
	}

	// plant: first time we know both the fieldprop AND what was actually
	// planted. Checked after the harvest branch above (not before, as in
	// earlier versions) so that a renewable node's repeat-harvest linkprop=0
	// signal - which also carries HasItemId, since the old item never leaves
	// the packet - is always claimed by the harvest branch first rather than
	// being misread as a fresh plant.
	if !plot.plantEmitted {
		if info.XML.HasItemId {
			// Renewable-node replant guard: swallow updates that land within
			// renewableReplantDebounce of this plot's last harvest - those are
			// the harvest transaction's own trailing echo packets, not a real
			// replant. Leaves plantEmitted false so the next (real) update is
			// re-evaluated from scratch once the debounce window passes. Note
			// this is a best-effort heuristic, not a verified signal (no raw
			// XML/tag capture has isolated a genuine replant from an ambient
			// continuation on a renewable node yet) - watch for two failure
			// modes empirically: replants still not firing "plant" (debounce
			// too generous), or "plant" firing spuriously on a renewable node
			// that regrew/was tended without a real replant (debounce too
			// short, or no replant signal exists on the wire at all).
			if !plot.harvestedAt.IsZero() && info.At.Sub(plot.harvestedAt) < renewableReplantDebounce {
				return nil
			}
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

	// tend: supportIndex genuinely reset to 0 (not just an ambient increment).
	if info.XML.HasSupportIndex && info.XML.SupportIndex == 0 && prevSupportIndex > 0 {
		return snapshot("tend")
	}

	// Ambient ticks (e.g. passive "grow"/"completed" repeats) - deliberately not emitted.
	return nil
}

// resetPlotForNextCycle clears plantEmitted/readyEmitted/harvestEmitted after
// a renewable node's harvest, so a genuine replant on the same fieldprop
// re-fires plant/ready instead of the plot going permanently "stuck" showing
// stale state (confirmed via live capture 2026-08-15: replanted Tree/Quartz/
// Spider plots never re-emitted plant/ready, only the next "tend" reflected
// the new cycle). plantEmitted is guarded separately by renewableReplantDebounce
// in the plant-detection branch above, to avoid the harvest transaction's own
// trailing "empty" state-echo packets (still carrying the old itemid, arriving
// within the same burst as the harvest) being misread as a fresh plant - an
// earlier version of this code reset plantEmitted unconditionally and hit
// exactly that false-positive.
func (f *FarmTracker) resetPlotForNextCycle(plot *plotState, at time.Time) {
	plot.harvestEmitted = false
	plot.readyEmitted = false
	plot.plantEmitted = false
	plot.harvestedAt = at
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
		Quality:      f.takeHarvestQuality(at),
	}

	delete(f.plots, fieldprop)
	for id, fp := range f.idToField {
		if fp == fieldprop {
			delete(f.idToField, id)
		}
	}

	return data
}
