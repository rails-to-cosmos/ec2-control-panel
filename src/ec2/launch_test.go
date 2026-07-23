package ec2

import "testing"

func TestResolvePersistentVolumeSize(t *testing.T) {
	const envDefault = 512
	size, src := ResolvePersistentVolumeSize(nil, envDefault)
	if size != envDefault || src != "EC2_VOLUME_SIZE" {
		t.Fatalf("no override: got (%d, %q)", size, src)
	}

	override := 1000
	size, src = ResolvePersistentVolumeSize(&override, envDefault)
	if size != 1000 || src != "volume_size" {
		t.Fatalf("override: got (%d, %q)", size, src)
	}

	// An explicit 0 is meaningless for an EBS volume — fall back rather than
	// asking AWS for a zero-sized disk.
	zero := 0
	if size, _ = ResolvePersistentVolumeSize(&zero, envDefault); size != envDefault {
		t.Fatalf("zero override should fall back, got %d", size)
	}
}
