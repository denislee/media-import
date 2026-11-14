package devices

import "testing"

func TestParseMountinfo(t *testing.T) {
	line := `120 30 8:33 / /run/media/dns/EOS\040DIGITAL rw,nosuid,nodev,relatime shared:70 - exfat /dev/sdc1 rw,uid=1000`
	v, ok := parseMountinfo(line)
	if !ok {
		t.Fatal("not parsed")
	}
	if v.Path != "/run/media/dns/EOS DIGITAL" || v.Label != "EOS DIGITAL" || v.FS != "exfat" {
		t.Fatalf("got %+v", v)
	}
	if !isCandidate(v.Path) || isCandidate("/home") {
		t.Fatal("candidate filter wrong")
	}
	if _, ok := parseMountinfo("garbage"); ok {
		t.Fatal("garbage parsed")
	}
}
