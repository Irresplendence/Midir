package packet

import (
	"fmt"
	"regexp"
	"strconv"
)

// PropUpdateInfo is the parsed form of a PropUpdate (0x52d2) packet.
// Observed structure: Msg = [String tag, Long 0, Byte flag, String xml, Float, Short]
type PropUpdateInfo struct {
	Id  uint64 // GamePacket.Id (may be the field prop id itself, or a linked seed/crop prop id)
	Tag string // "seed", "single", "grow", "collecting", etc.
	XML PropXMLAttrs
}

// PropAppearInfo is the parsed form of a PropAppears (0x52d0) packet.
// Observed structure: Msg = [Long linkId, Int, String, String, Bin, String tag, Long 0, Byte, String xml, Int, Short]
// The XML at appear-time only ever carries "onwer" (owner) - no fieldprop/itemid yet.
type PropAppearInfo struct {
	Id     uint64 // GamePacket.Id
	LinkId uint64 // Msg[0], the linked seed/crop prop id
	Tag    string
	XML    PropXMLAttrs
}

// PropXMLAttrs holds the fields extracted from the embedded
// `<xml onwer="..." fieldprop="..." .../>` string. Note: "onwer" is a literal
// typo in the wire protocol, not a bug in this parser.
type PropXMLAttrs struct {
	Owner           uint64
	HasFieldProp    bool
	FieldProp       uint64
	HasLinkProp     bool
	LinkProp        uint64
	HasItemId       bool
	ItemId          uint64
	Support         uint64
	SupportIndex    uint64
	HasSupportIndex bool
	Special         bool
	Fertility       bool
	StartTime       uint64
	LMTime          uint64
}

var propXMLAttrRe = regexp.MustCompile(`(\w+)="([^"]*)"`)

func parsePropXML(xml string) PropXMLAttrs {
	var a PropXMLAttrs
	for _, m := range propXMLAttrRe.FindAllStringSubmatch(xml, -1) {
		key, val := m[1], m[2]
		switch key {
		case "onwer", "owner":
			a.Owner, _ = strconv.ParseUint(val, 10, 64)
		case "fieldprop":
			a.FieldProp, _ = strconv.ParseUint(val, 10, 64)
			a.HasFieldProp = true
		case "linkprop":
			a.LinkProp, _ = strconv.ParseUint(val, 10, 64)
			a.HasLinkProp = true
		case "itemid":
			a.ItemId, _ = strconv.ParseUint(val, 10, 64)
			a.HasItemId = true
		case "support":
			a.Support, _ = strconv.ParseUint(val, 10, 64)
		case "supportIndex":
			a.SupportIndex, _ = strconv.ParseUint(val, 10, 64)
			a.HasSupportIndex = true
		case "Special":
			a.Special = val == "true"
		case "Fertility":
			a.Fertility = val == "true"
		case "starttime":
			a.StartTime, _ = strconv.ParseUint(val, 10, 64)
		case "lmtime":
			a.LMTime, _ = strconv.ParseUint(val, 10, 64)
		}
	}
	return a
}

// ParsePropUpdatePacket parses a PropUpdate (0x52d2) packet.
func ParsePropUpdatePacket(p *GamePacket) (*PropUpdateInfo, error) {
	msg := p.Msg
	if len(msg) < 4 {
		return nil, fmt.Errorf("prop update packet too short: %d elems", len(msg))
	}
	if msg[0].Type() != MessageElemTypeString {
		return nil, fmt.Errorf("prop update tag has unexpected type %v", msg[0].Type())
	}
	if msg[3].Type() != MessageElemTypeString {
		return nil, fmt.Errorf("prop update xml has unexpected type %v", msg[3].Type())
	}

	return &PropUpdateInfo{
		Id:  p.Id,
		Tag: msg[0].Data().(string),
		XML: parsePropXML(msg[3].Data().(string)),
	}, nil
}

// PropDisappearInfo is the parsed form of a PropDisappears (0x52d1) packet.
// Observed structure: Msg = [Long linkId] - the same [Id, linkId] shape as
// PropAppears, just without a tag/xml payload.
type PropDisappearInfo struct {
	Id     uint64 // GamePacket.Id
	LinkId uint64 // Msg[0]
}

// ParsePropDisappearPacket parses a PropDisappears (0x52d1) packet.
func ParsePropDisappearPacket(p *GamePacket) (*PropDisappearInfo, error) {
	msg := p.Msg
	if len(msg) < 1 {
		return nil, fmt.Errorf("prop disappear packet too short")
	}
	if msg[0].Type() != MessageElemTypeLong {
		return nil, fmt.Errorf("prop disappear linkId has unexpected type %v", msg[0].Type())
	}

	return &PropDisappearInfo{
		Id:     p.Id,
		LinkId: msg[0].Data().(uint64),
	}, nil
}

// ParsePropAppearPacket parses a PropAppears (0x52d0) packet.
func ParsePropAppearPacket(p *GamePacket) (*PropAppearInfo, error) {
	msg := p.Msg
	if len(msg) < 9 {
		return nil, fmt.Errorf("prop appear packet too short: %d elems", len(msg))
	}
	if msg[0].Type() != MessageElemTypeLong {
		return nil, fmt.Errorf("prop appear linkId has unexpected type %v", msg[0].Type())
	}
	if msg[5].Type() != MessageElemTypeString {
		return nil, fmt.Errorf("prop appear tag has unexpected type %v", msg[5].Type())
	}
	if msg[8].Type() != MessageElemTypeString {
		return nil, fmt.Errorf("prop appear xml has unexpected type %v", msg[8].Type())
	}

	return &PropAppearInfo{
		Id:     p.Id,
		LinkId: msg[0].Data().(uint64),
		Tag:    msg[5].Data().(string),
		XML:    parsePropXML(msg[8].Data().(string)),
	}, nil
}
