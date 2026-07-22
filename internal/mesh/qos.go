package mesh

// Quality of service: classify outbound overlay packets into priority classes
// so the egress shaper can favour important traffic when the link is saturated.
// Class 0 is the highest priority. QoS only has an effect when an up-throttle is
// configured (no rate cap means no queue to reorder).

// ClassRule is an exported, config-friendly classification rule.
type ClassRule struct {
	Proto   uint8  // 0=any, 1=icmp, 6=tcp, 17=udp
	PortMin uint16 // 0,0 = any port
	PortMax uint16
	DSCP    int // -1 = any, else match DSCP value (0-63)
	Class   int // target priority class (0 = highest)
}

// NewClassifier builds a classifier. classes is the number of priority levels;
// defClass is where unmatched traffic goes. classDSCP is an optional
// per-class outbound marking override (see DefaultClassDSCP); pass nil to use
// the standard-codepoint default for every class.
func NewClassifier(classes, defClass int, rules []ClassRule, classDSCP []int) *classifier {
	if classes < 1 {
		classes = 1
	}
	if defClass < 0 || defClass >= classes {
		defClass = classes - 1
	}
	rs := make([]qosRule, 0, len(rules))
	for _, r := range rules {
		c := r.Class
		if c < 0 || c >= classes {
			c = defClass
		}
		rs = append(rs, qosRule{proto: r.Proto, portMin: r.PortMin, portMax: r.PortMax, dscp: r.DSCP, class: c})
	}
	return &classifier{rules: rs, classes: classes, defClass: defClass, classDSCP: classDSCP}
}

type qosRule struct {
	proto   uint8
	portMin uint16
	portMax uint16
	dscp    int // -1 = any
	class   int
}

func (r qosRule) match(proto uint8, sp, dp uint16, dscp int) bool {
	if r.proto != 0 && r.proto != proto {
		return false
	}
	if r.dscp >= 0 && r.dscp != dscp {
		return false
	}
	if r.portMin != 0 || r.portMax != 0 {
		inRange := func(p uint16) bool { return p >= r.portMin && p <= r.portMax }
		if !inRange(sp) && !inRange(dp) {
			return false
		}
	}
	return true
}

type classifier struct {
	rules    []qosRule
	classes  int
	defClass int

	// classDSCP is an optional per-class outbound marking override, indexed
	// by class. An entry that's missing (index >= len) or negative falls
	// back to DefaultClassDSCP for that class. nil means "no overrides at
	// all" — every class uses the standard-codepoint default.
	classDSCP []int
}

func (c *classifier) numClasses() int {
	if c == nil || c.classes < 1 {
		return 1
	}
	return c.classes
}

func (c *classifier) classify(pkt []byte) int {
	if c == nil {
		return 0
	}
	proto, sp, dp, dscp, ok := parseL4(pkt)
	if !ok {
		return c.defClass
	}
	for _, r := range c.rules {
		if r.match(proto, sp, dp, dscp) {
			return r.class
		}
	}
	return c.defClass
}

// dscpFor returns the outbound DSCP mark for a class: the configured
// override if one was set for it, else the standard-codepoint default for
// its position among the classifier's priority levels.
func (c *classifier) dscpFor(class int) int {
	if c == nil {
		return -1
	}
	if class >= 0 && class < len(c.classDSCP) && c.classDSCP[class] >= 0 {
		return c.classDSCP[class]
	}
	return DefaultClassDSCP(class, c.classes, c.defClass)
}

// markDSCP rewrites pkt's DSCP field in place to match class's outbound
// mark. A nil classifier leaves pkt untouched — QoS being off means no
// marking either, same as it means no classification.
func (c *classifier) markDSCP(pkt []byte, class int) {
	if c == nil {
		return
	}
	setDSCP(pkt, c.dscpFor(class))
}

// DefaultClassDSCP maps a class index to a standard Diffserv DSCP codepoint
// based on its position among classes priority levels: EF (expedited
// forwarding, 46) for the highest class (0), CS0 (0, the ordinary "no
// particular treatment" default) for whichever class unmatched traffic
// actually lands in (defClass — marking default traffic with anything other
// than the standard "default" codepoint would be misleading), and CS1 (8,
// the conventional "scavenger"/below-default class) for the lowest class.
// Classes strictly between highest and defClass step down through the
// Assured-Forwarding low-drop-precedence codepoints (AF41/AF31/AF21/AF11);
// classes between defClass and lowest ramp linearly from CS0 to CS1, since
// Diffserv doesn't define standard codepoints for "worse than default but
// better than scavenger."
func DefaultClassDSCP(class, classes, defClass int) int {
	if classes <= 1 {
		return 0 // CS0
	}
	if class < 0 {
		class = 0
	}
	if class > classes-1 {
		class = classes - 1
	}
	if defClass <= 0 || defClass >= classes-1 {
		// defClass sits at (or outside) an anchor already used by
		// highest/lowest, leaving no room for it to mark a distinct
		// "middle" position — fall back to the geometric middle so a
		// multi-class setup still gets more than two distinct marks.
		defClass = classes / 2
		if defClass == 0 {
			defClass = 1
		}
	}
	switch {
	case class == 0:
		return 46 // EF
	case class == defClass:
		return 0 // CS0
	case class == classes-1:
		return 8 // CS1
	case class < defClass:
		af := [...]int{34, 26, 18, 10} // AF41, AF31, AF21, AF11
		idx := (class - 1) * len(af) / defClass
		if idx >= len(af) {
			idx = len(af) - 1
		}
		return af[idx]
	default: // defClass < class < classes-1
		span := classes - 1 - defClass
		idx := class - defClass
		return idx * 8 / span // linear ramp CS0(0) -> CS1(8)
	}
}

// parseL4 extracts protocol, ports, and DSCP from an IPv4/IPv6 packet. IPv6
// extension headers are not walked (the common no-extension case is handled).
func parseL4(pkt []byte) (proto uint8, srcPort, dstPort uint16, dscp int, ok bool) {
	if len(pkt) < 1 {
		return 0, 0, 0, 0, false
	}
	switch pkt[0] >> 4 {
	case 4:
		if len(pkt) < 20 {
			return 0, 0, 0, 0, false
		}
		ihl := int(pkt[0]&0x0f) * 4
		if ihl < 20 || len(pkt) < ihl {
			return 0, 0, 0, 0, false
		}
		dscp = int(pkt[1] >> 2)
		proto = pkt[9]
		if (proto == 6 || proto == 17) && len(pkt) >= ihl+4 {
			srcPort = uint16(pkt[ihl])<<8 | uint16(pkt[ihl+1])
			dstPort = uint16(pkt[ihl+2])<<8 | uint16(pkt[ihl+3])
		}
		return proto, srcPort, dstPort, dscp, true
	case 6:
		if len(pkt) < 40 {
			return 0, 0, 0, 0, false
		}
		tc := (pkt[0]&0x0f)<<4 | (pkt[1] >> 4)
		dscp = int(tc >> 2)
		proto = pkt[6] // next header (no extension-header walk)
		if (proto == 6 || proto == 17) && len(pkt) >= 44 {
			srcPort = uint16(pkt[40])<<8 | uint16(pkt[41])
			dstPort = uint16(pkt[42])<<8 | uint16(pkt[43])
		}
		return proto, srcPort, dstPort, dscp, true
	}
	return 0, 0, 0, 0, false
}

// setDSCP rewrites pkt's DSCP field in place to the given 6-bit value
// (0-63), the write-side mirror of the DSCP bits parseL4 reads. It preserves
// the 2 ECN bits alongside DSCP in the same byte/traffic-class field rather
// than clobbering them, and — for IPv4 only — fixes up the header checksum,
// which covers those bits; IPv6 has no header checksum, and neither family's
// TCP/UDP checksum covers DSCP/ECN (the pseudo-header is just addresses,
// protocol, and length), so nothing else needs recomputing. A negative dscp,
// or one outside 0-63, leaves pkt untouched.
func setDSCP(pkt []byte, dscp int) {
	if dscp < 0 || dscp > 63 || len(pkt) < 1 {
		return
	}
	switch pkt[0] >> 4 {
	case 4:
		if len(pkt) < 20 {
			return
		}
		ihl := int(pkt[0]&0x0f) * 4
		if ihl < 20 || len(pkt) < ihl {
			return
		}
		ecn := pkt[1] & 0x03
		newTOS := byte(dscp<<2) | ecn
		if pkt[1] == newTOS {
			return // already marked; skip the checksum walk
		}
		pkt[1] = newTOS
		pkt[10], pkt[11] = 0, 0
		c := ones(pkt[:ihl], 0)
		pkt[10], pkt[11] = byte(c>>8), byte(c)
	case 6:
		if len(pkt) < 2 {
			return
		}
		ecn := ((pkt[0]&0x0f)<<4 | (pkt[1] >> 4)) & 0x03
		tc := byte(dscp<<2) | ecn
		pkt[0] = (pkt[0] & 0xf0) | (tc >> 4)
		pkt[1] = (pkt[1] & 0x0f) | (tc << 4)
		// IPv6 has no header checksum to fix up.
	}
}
