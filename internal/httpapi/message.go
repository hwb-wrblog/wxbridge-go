package httpapi

import (
	"encoding/xml"
	"io"
	"strings"

	"feiyu.com/wx/internal/workflow"
	"feiyu.com/wx/protobuf/wechat"
)

func projectMessage(message *wechat.AddMsg, self string) map[string]interface{} {
	from, to := message.GetFromUserName().GetStr(), message.GetToUserName().GetStr()
	conversation, sender, content := from, from, message.GetContent().GetStr()
	group := strings.HasSuffix(from, "@chatroom") || strings.HasSuffix(to, "@chatroom")
	if strings.HasSuffix(from, "@chatroom") {
		// Incoming group messages carry the member ID before the message body.
		sender = ""
		if member, body, ok := groupMessageBody(content); ok {
			sender, content = member, body
		}
	} else if strings.HasSuffix(to, "@chatroom") {
		conversation = to
	} else if self != "" && from == self {
		conversation = to
	}
	direction := "incoming"
	if self != "" && sender == self {
		direction = "outgoing"
	}
	text := ""
	if message.GetMsgType() == 1 {
		text = content
	}
	return map[string]interface{}{
		"id":              workflow.MessageID(message),
		"from":            from,
		"to":              to,
		"type":            message.GetMsgType(),
		"created":         message.GetCreateTime(),
		"conversation_id": conversation,
		"sender_id":       sender,
		"direction":       direction,
		"is_group":        group,
		"mentions_me":     group && direction == "incoming" && sourceMentions(message.GetMsgSource(), self),
		"text":            text,
	}
}

func groupMessageBody(content string) (string, string, bool) {
	member, body, ok := strings.Cut(content, ":\n")
	if !ok || len(member) == 0 || len(member) > 128 {
		return "", content, false
	}
	for _, c := range member {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-' || c == '.') {
			return "", content, false
		}
	}
	return member, body, true
}

func sourceMentions(raw, self string) bool {
	if self == "" || len(raw) == 0 || len(raw) > 64<<10 {
		return false
	}
	var source struct {
		XMLName xml.Name `xml:"msgsource"`
		AtUsers []string `xml:"atuserlist"`
	}
	decoder := xml.NewDecoder(strings.NewReader(raw))
	if err := decoder.Decode(&source); err != nil {
		return false
	}
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		space, ok := token.(xml.CharData)
		if err != nil || !ok || strings.TrimSpace(string(space)) != "" {
			return false
		}
	}
	for _, list := range source.AtUsers {
		for _, id := range strings.Split(list, ",") {
			id = strings.TrimSpace(id)
			if id == self || id == "notify@all" {
				return true
			}
		}
	}
	return false
}
