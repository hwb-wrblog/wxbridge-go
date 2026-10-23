package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"feiyu.com/wx/internal/workflow"
)

func main() {
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: wechat-flow [-state DIR] serve|qr|check|login|watch|status|heartbeat|sync|contact|send|image|emoji|gif|voice|video|download [command flags]")
		flag.PrintDefaults()
	}
	dir := flag.String("state", "../workflow-runtime", "private session directory")
	flag.Parse()
	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}
	dirAbs, err := filepath.Abs(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.MkdirAll(dirAbs, 0700); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	lock, err := os.OpenFile(filepath.Join(dirAbs, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		fmt.Fprintln(os.Stderr, "another workflow command is using this session")
		os.Exit(1)
	}
	// Legacy SDK debug output can contain session material. Keep it in a
	// private file and reserve the original stdout for the structured result.
	resultFD, err := syscall.Dup(int(os.Stdout.Fd()))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	resultOut := os.NewFile(uintptr(resultFD), "result")
	defer resultOut.Close()
	log, err := os.OpenFile(filepath.Join(dirAbs, "sdk.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer log.Close()
	if err := syscall.Dup3(int(log.Fd()), 1, 0); err != nil {
		fmt.Fprintln(resultOut, err)
		os.Exit(1)
	}
	if err := syscall.Dup3(int(log.Fd()), 2, 0); err != nil {
		fmt.Fprintln(resultOut, err)
		os.Exit(1)
	}
	result := map[string]interface{}{"ok": false, "command": flag.Arg(0), "source": "recovered-source workflow"}
	defer func() {
		if p := recover(); p != nil {
			result["error"] = fmt.Sprintf("historical SDK panic: %v (see private sdk.log)", p)
		}
		json.NewEncoder(resultOut).Encode(result)
		if result["ok"] != true {
			os.Exit(1)
		}
	}()
	s, err := workflow.Open(dirAbs)
	if err != nil {
		result["error"] = err.Error()
		return
	}
	result["client_version"] = fmt.Sprintf("%#x", workflow.ClientVersion)
	if err := run(s, flag.Args(), result); err != nil {
		result["error"] = err.Error()
		return
	}
	result["ok"] = true
}

func run(s *workflow.Session, args []string, out map[string]interface{}) error {
	if len(args) == 0 {
		return errors.New("missing command")
	}
	if args[0] != "send" && args[0] != "contact" && args[0] != "serve" && args[0] != "download" && !isMediaCommand(args[0]) && len(args) != 1 {
		return fmt.Errorf("%s does not accept additional arguments", args[0])
	}
	switch args[0] {
	case "image", "emoji", "gif", "voice", "video":
		return runMedia(s, args, out)
	case "serve":
		return runServer(s, args[1:])
	case "download":
		flags := flag.NewFlagSet("download", flag.ContinueOnError)
		id := flags.String("id", "", "saved media message ID")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("unexpected download arguments")
		}
		file, err := s.DownloadMedia(*id)
		out["media"] = file
		return err
	case "qr":
		path, err := s.QR()
		if err != nil {
			return err
		}
		out["image_path"], out["expires"] = path, s.State.Expires
	case "check":
		state, err := s.Check()
		out["qr_state"] = state
		return err
	case "login":
		if err := s.Login(); err != nil {
			return err
		}
		out["authenticated"], out["wxid"] = true, s.State.User.WxId
	case "watch":
		deadline := time.Now().Add(4 * time.Minute)
		for time.Now().Before(deadline) {
			state, err := s.Check()
			if err != nil {
				return err
			}
			if state == 2 {
				if err := s.Login(); err != nil {
					return err
				}
				out["authenticated"], out["wxid"] = true, s.State.User.WxId
				return nil
			}
			if state == 4 {
				return errors.New("QR expired; run qr again")
			}
			time.Sleep(2 * time.Second)
		}
		return errors.New("timed out waiting for phone confirmation")
	case "status":
		out["authenticated"] = s.State.Authenticated
		out["qr_state"] = s.State.QRState
		out["pending_messages"] = len(s.State.Pending)
		out["status_source"] = "local saved state; online status not checked"
		out["client_version"] = fmt.Sprintf("%#x", workflow.ClientVersion)
		if s.State.User != nil && s.State.User.DeviceInfo != nil {
			out["device_token_ready"] = s.State.User.DeviceInfo.DeviceToken.GetTrustResponseData().GetDeviceToken() != ""
		}
		if s.State.Authenticated {
			out["wxid"] = s.State.User.WxId
		}
	case "heartbeat":
		if err := s.Heartbeat(); err != nil {
			return err
		}
		out["online"] = true
	case "contact":
		flags := flag.NewFlagSet("contact", flag.ContinueOnError)
		username := flags.String("id", "", "exact contact username")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("unexpected contact arguments")
		}
		contact, err := s.Contact(*username)
		out["contact"] = contact
		return err
	case "send":
		flags := flag.NewFlagSet("send", flag.ContinueOnError)
		to, content := flags.String("to", "", "recipient"), flags.String("text", "", "message text")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("unexpected send arguments")
		}
		response, err := s.Send(*to, *content)
		out["response"] = response
		return err
	case "sync":
		msgs, more, err := s.Sync()
		if err != nil {
			return err
		}
		out["messages"], out["more"] = msgs, more
		out["saved_messages"] = len(s.State.Pending)
	default:
		return errors.New("unknown command")
	}
	return nil
}
