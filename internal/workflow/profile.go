package workflow

import "feiyu.com/wx/clientsdk/baseinfo"

// ClientVersion is the profile verified with a fresh QR, secmanualauth,
// heartbeat, text sending, and message synchronization on 2026-10-23.
const ClientVersion uint32 = 0x18004b2f

func configureClient() {
	baseinfo.ClientVersion = ClientVersion
	baseinfo.ServerVersion = ClientVersion
	baseinfo.PlistVersion = ClientVersion
	baseinfo.DeviceTypeIos = "iPad"
	baseinfo.DeviceVersionNumber = "8.0.75(0x18004b2f)"
}
