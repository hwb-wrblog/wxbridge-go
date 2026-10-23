package workflow

import (
	"errors"

	"feiyu.com/wx/clientsdk"
	"feiyu.com/wx/protobuf/wechat"
)

type ContactIdentity struct {
	Username string `json:"username"`
	Nickname string `json:"nickname"`
	Remark   string `json:"remark"`
}

func (s *Session) Contact(username string) (*ContactIdentity, error) {
	if err := s.prepareMedia(username); err != nil {
		return nil, err
	}
	header, err := clientsdk.SendGetContactRequest(s.State.User, []string{username}, nil, nil)
	if err != nil {
		return nil, err
	}
	response := &wechat.GetContactResponse{}
	if err := clientsdk.ParseResponseData(s.State.User, header, response); err != nil {
		return nil, err
	}
	if err := responseOK(response.GetBaseResponse()); err != nil {
		return nil, err
	}
	if response.GetContactCount() != 1 || len(response.GetContactList()) != 1 {
		return nil, errors.New("contact response does not contain exactly one contact")
	}
	contact := response.GetContactList()[0]
	if contact.GetUserName().GetStr() != username {
		return nil, errors.New("contact response username mismatch")
	}
	if err := s.Save(); err != nil {
		return nil, err
	}
	return &ContactIdentity{Username: username, Nickname: contact.GetNickName().GetStr(), Remark: contact.GetRemark().GetStr()}, nil
}
