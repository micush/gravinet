//go:build !darwin && !openbsd

package mesh

// platformPMTUCeil is only meaningful on darwin and openbsd (see
// pmtu_cap_darwin.go and pmtu_cap_openbsd.go); on every other platform
// IP_DONTFRAG/IPV6_DONTFRAG actually work for both families, so this is set
// high enough to never constrain discovery.
const platformPMTUCeil = 1 << 30
