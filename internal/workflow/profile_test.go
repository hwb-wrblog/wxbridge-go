package workflow

import (
	"testing"

	"feiyu.com/wx/clientsdk/baseinfo"
)

func TestWorkingProfilePersistsAndRejectsLegacyState(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.State.ClientVersion != ClientVersion || baseinfo.ClientVersion != ClientVersion || baseinfo.ServerVersion != ClientVersion || baseinfo.PlistVersion != ClientVersion {
		t.Fatal("new session does not use the verified profile across request layers")
	}
	s.State.User = &baseinfo.UserInfo{DeviceInfo: &baseinfo.DeviceInfo{}}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil || s.State.ClientVersion != ClientVersion {
		t.Fatalf("working session did not reload: %v", err)
	}
	for _, legacyVersion := range []uint32{0, 0x18003e2b} {
		s.State.ClientVersion = legacyVersion
		if err := s.Save(); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(dir); err == nil {
			t.Fatal("legacy state accepted by the verified-only workflow")
		}
	}
}
