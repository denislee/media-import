package devices

import "testing"

func TestParseMountinfo(t *testing.T) {
	line := `120 30 8:33 / /run/media/dns/EOS\040DIGITAL rw,nosuid,nodev,relatime shared:70 - exfat /dev/sdc1 rw,uid=1000`
	v, ok := parseMountinfo(line)
	if !ok {
		t.Fatal("not parsed")
	}
	if v.Path != "/run/media/dns/EOS DIGITAL" || v.Label != "EOS DIGITAL" || v.FS != "exfat" || v.Dev != "/dev/sdc1" {
		t.Fatalf("got %+v", v)
	}
	if !isCandidate(v.Path) || isCandidate("/home") {
		t.Fatal("candidate filter wrong")
	}
	if _, ok := parseMountinfo("garbage"); ok {
		t.Fatal("garbage parsed")
	}
}

func TestParseLsblk(t *testing.T) {
	data := []byte(`{"blockdevices":[
	 {"name":"sda","path":"/dev/sda","label":null,"fstype":null,"mountpoints":[null],"rm":true,"hotplug":true,"tran":"usb","type":"disk","size":32010928128,
	  "children":[
	   {"name":"sda1","path":"/dev/sda1","label":"SD_Card","fstype":"exfat","mountpoints":[null],"rm":true,"hotplug":true,"tran":null,"type":"part","size":32010895360},
	   {"name":"sda2","path":"/dev/sda2","label":"M","fstype":"vfat","mountpoints":["/run/media/u/M"],"rm":true,"hotplug":true,"tran":null,"type":"part","size":1024}
	  ]},
	 {"name":"nvme0n1","path":"/dev/nvme0n1","label":null,"fstype":null,"mountpoints":[],"rm":false,"hotplug":false,"tran":"nvme","type":"disk","size":1000,
	  "children":[{"name":"nvme0n1p1","path":"/dev/nvme0n1p1","label":"EFI","fstype":"vfat","mountpoints":[],"rm":false,"hotplug":false,"tran":null,"type":"part","size":1000}]},
	 {"name":"zram0","path":"/dev/zram0","label":"zram0","fstype":"swap","mountpoints":["[SWAP]"],"rm":false,"hotplug":false,"tran":null,"type":"disk","size":4096}
	]}`)
	got := parseLsblk(data)
	if len(got) != 1 || got[0].Dev != "/dev/sda1" || got[0].Label != "SD_Card" || got[0].FS != "exfat" {
		t.Fatalf("got %+v", got)
	}
	if mountName("a/b") != "a_b" || mountName("..") != "disk" {
		t.Fatal("mountName")
	}
}
