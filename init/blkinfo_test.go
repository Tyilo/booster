package main

import (
	"bytes"
	"encoding/hex"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func check(t *testing.T, name, fstype, uuidStr, label string, size int64, script string) {
	if !fileExists("assets") {
		if err := os.Mkdir("assets", 0755); err != nil {
			t.Fatal(err)
		}
	}

	asset := "assets/" + name
	if !fileExists(asset) {
		script = strings.ReplaceAll(script, "$OUTPUT", asset)
		script = strings.ReplaceAll(script, "$UUID", uuidStr)
		script = strings.ReplaceAll(script, "$LABEL", label)

		f, err := os.Create(asset)
		if err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
		if err := os.Truncate(asset, size*1024*1024); err != nil {
			t.Fatal(err)
		}

		if err := shell(script); err != nil {
			_ = os.Remove(asset)
			t.Fatal(err)
		}
	}

	info, err := readBlkInfo(asset)
	if err != nil {
		t.Fatalf("%s: %v", fstype, err)
	}
	if info.format != fstype {
		t.Errorf("blkinfo(%s) format = %v, want %v", asset, info.format, fstype)
	}
	var uuid []byte
	if fstype == "mbr" {
		uuid, err = hex.DecodeString(uuidStr)
	} else {
		uuid, err = parseUUID(uuidStr)
	}
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(info.uuid, uuid) {
		t.Errorf("blkinfo(%s) uuid = %v, want %v", fstype, info.uuid.toString(), uuidStr)
	}
	if info.label != label {
		t.Errorf("blkinfo(%s) label = %v, want %v", fstype, info.label, label)
	}
}

func shell(script string, env ...string) error {
	sh := exec.Command("bash", "-o", "errexit", "-c", script)
	sh.Env = append(os.Environ(), env...)

	if testing.Verbose() {
		sh.Stdout = os.Stdout
		sh.Stderr = os.Stderr
	}
	return sh.Run()
}

func fileExists(file string) bool {
	_, err := os.Stat(file)
	return err == nil
}

func TestBlkInfo(t *testing.T) {
	check(t, "ext4", "ext4", "717be5ba-d42d-4aaa-b846-8a23cc7471b0", "extlabel", 10, "mkfs.ext4 -L $LABEL -U $UUID $OUTPUT")
	check(t, "btrfs", "btrfs", "1884e1eb-186f-4b1b-af11-45ea80da8e3c", "btrfs111", 200, "mkfs.btrfs -L $LABEL -U $UUID $OUTPUT")
	check(t, "xfs", "xfs", "ee7cad9a-0202-4c00-a320-418a9276d70d", "xfs44", 100, "mkfs.xfs -L $LABEL -m uuid=$UUID $OUTPUT")
	check(t, "f2fs", "f2fs", "6af49bb0-0bd8-4b82-a1d1-286dfe37d729", "test1", 100, "mkfs.f2fs -l $LABEL -U $UUID $OUTPUT")
	check(t, "luks1", "luks", "6faf1e59-9999-4da4-97f9-c815e7353777", "", 100, "cryptsetup luksFormat -q --type=luks1 --iter-time=1 --uuid=$UUID $OUTPUT <<< 'tetspassphrase'")
	check(t, "luks2", "luks", "51df71ed-8e4a-4a7a-956d-b782706a52d1", "bazz", 10, "cryptsetup luksFormat -q --type=luks2 --iter-time=1 --uuid=$UUID --label=$LABEL $OUTPUT <<< 'tetspassphrase'")
	check(t, "gpt", "gpt", "c26fcabe-8010-4bff-a066-8c73e76dbb32", "", 1, "fdisk $OUTPUT <<< 'g\nx\ni\n$UUID\nr\nw\n'")
	check(t, "mbr", "mbr", "2beab180", "", 1, "fdisk $OUTPUT <<< 'o\nx\ni\n0x$UUID\nr\nw\n'")
}
