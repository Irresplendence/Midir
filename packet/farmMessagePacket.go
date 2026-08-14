package packet

import (
	"fmt"
	"regexp"
)

var plantMessageRe = regexp.MustCompile(`^You used (.+?) \([^)]*\)!`)

// ParseFarmSystemMessage parses a generic system message packet (0x526d) and,
// if it matches the plant-confirmation shape ("You used <item> (<farm>)!..."),
// returns the planted item's display name (e.g. "Blackberry Seeds",
// "Rubber Tree Nutrients"). This opcode is shared by several unrelated
// message types (e.g. "Your tending succeeded!"), so ok=false is the expected,
// normal result for anything that isn't a plant confirmation - callers should
// not treat it as an error.
func ParseFarmSystemMessage(p *GamePacket) (name string, ok bool, err error) {
	msg := p.Msg
	if len(msg) < 2 {
		return "", false, fmt.Errorf("system message packet too short")
	}
	if msg[1].Type() != MessageElemTypeString {
		return "", false, fmt.Errorf("system message text has unexpected type %v", msg[1].Type())
	}

	text := msg[1].Data().(string)
	m := plantMessageRe.FindStringSubmatch(text)
	if m == nil {
		return "", false, nil
	}
	return m[1], true, nil
}

var harvestStorageMessageRe = regexp.MustCompile(`^(Common|Fine|Finest) (.+?) \([^)]*\) x\d+ has been placed in storage\.`)

// HarvestStorageInfo is the parsed form of a harvest-confirmation message
// (opcode 0x213a6, "<Quality> <Item> (<farm>) x<n> has been placed in
// storage."). This is where quality (Common/Fine/Finest) actually lives on
// the wire - there is no separate numeric quality/score field anywhere in the
// PropUpdate XML.
type HarvestStorageInfo struct {
	Quality string // "Common" | "Fine" | "Finest"
	Name    string // e.g. "Blackberry", "Magic Cobweb" - the harvested product name, not the seed name
}

// ParseHarvestStorageMessage parses a 0x213a6 packet. ok=false (not an error)
// if the message text doesn't match the storage-confirmation shape.
func ParseHarvestStorageMessage(p *GamePacket) (info *HarvestStorageInfo, ok bool, err error) {
	msg := p.Msg
	if len(msg) < 3 {
		return nil, false, fmt.Errorf("harvest storage message packet too short")
	}
	if msg[2].Type() != MessageElemTypeString {
		return nil, false, fmt.Errorf("harvest storage message text has unexpected type %v", msg[2].Type())
	}

	text := msg[2].Data().(string)
	m := harvestStorageMessageRe.FindStringSubmatch(text)
	if m == nil {
		return nil, false, nil
	}
	return &HarvestStorageInfo{Quality: m[1], Name: m[2]}, true, nil
}
