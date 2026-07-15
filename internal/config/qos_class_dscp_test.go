package config

import "testing"

func TestQoSSetClassDSCP(t *testing.T) {
	c := &Config{PrimaryPort: 65432, EnableIPv4: true,
		Networks: []Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24"}}}

	// Setting a class's mark before any QoS config exists should seed the
	// 5-class default (same as QoSAdd does) rather than erroring.
	if err := c.QoSSetClassDSCP("lan", 0, 46); err != nil {
		t.Fatal(err)
	}
	n := c.Networks[0]
	if n.QoS.Classes != 5 {
		t.Fatalf("classes = %d, want seeded default 5", n.QoS.Classes)
	}
	if len(n.QoS.ClassDSCP) < 1 || n.QoS.ClassDSCP[0] != 46 {
		t.Fatalf("class_dscp = %v, want [46, ...]", n.QoS.ClassDSCP)
	}

	// Setting a later class extends the slice, backfilling earlier unset
	// entries with -1 (meaning "no override, use the default") rather than
	// zero (which would be misread as an override to CS0).
	if err := c.QoSSetClassDSCP("lan", 3, 0); err != nil {
		t.Fatal(err)
	}
	n = c.Networks[0]
	if len(n.QoS.ClassDSCP) != 4 {
		t.Fatalf("class_dscp length = %d, want 4", len(n.QoS.ClassDSCP))
	}
	for _, cl := range []int{1, 2} {
		if n.QoS.ClassDSCP[cl] != -1 {
			t.Errorf("class %d should be backfilled as -1 (unset), got %d", cl, n.QoS.ClassDSCP[cl])
		}
	}
	if n.QoS.ClassDSCP[3] != 0 {
		t.Fatalf("class 3 = %d, want 0 (CS0 is a valid explicit override)", n.QoS.ClassDSCP[3])
	}

	// Out-of-range class or DSCP value errors without mutating anything.
	if err := c.QoSSetClassDSCP("lan", 99, 10); err == nil {
		t.Error("out-of-range class should error")
	}
	if err := c.QoSSetClassDSCP("lan", 0, 64); err == nil {
		t.Error("out-of-range dscp (>63) should error")
	}
	if err := c.QoSSetClassDSCP("lan", 0, -1); err == nil {
		t.Error("negative dscp should error")
	}
}

func TestQoSClearClassDSCP(t *testing.T) {
	c := &Config{PrimaryPort: 65432, EnableIPv4: true,
		Networks: []Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24"}}}
	if err := c.QoSSetClassDSCP("lan", 2, 12); err != nil {
		t.Fatal(err)
	}
	if err := c.QoSClearClassDSCP("lan", 2); err != nil {
		t.Fatal(err)
	}
	if got := c.Networks[0].QoS.ClassDSCP[2]; got != -1 {
		t.Fatalf("class 2 after clear = %d, want -1 (reverted to default)", got)
	}
	// Clearing a class with no override, or one that was never touched, errors.
	if err := c.QoSClearClassDSCP("lan", 2); err == nil {
		t.Error("clearing an already-cleared override should error")
	}
	if err := c.QoSClearClassDSCP("lan", 4); err == nil {
		t.Error("clearing a class with no entry at all should error")
	}
}
