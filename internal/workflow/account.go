package workflow

type AccountIdentity struct {
	Authenticated bool   `json:"authenticated"`
	WxID          string `json:"wxid"`
	Nickname      string `json:"nickname"`
	AvatarURL     string `json:"avatar_url"`
}

func (s *Session) Identity() AccountIdentity {
	if !s.State.Authenticated || s.State.User == nil {
		return AccountIdentity{}
	}
	u := s.State.User
	return AccountIdentity{Authenticated: true, WxID: u.WxId, Nickname: u.NickName, AvatarURL: u.HeadURL}
}
