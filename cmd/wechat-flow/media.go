package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"feiyu.com/wx/internal/workflow"
)

func isMediaCommand(command string) bool {
	switch command {
	case "image", "emoji", "gif", "voice", "video":
		return true
	}
	return false
}

func readMediaFile(path string, limit int) ([]byte, error) {
	if path == "" {
		return nil, errors.New("a media file path is required")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !stat.Mode().IsRegular() || stat.Size() == 0 || stat.Size() > int64(limit) {
		return nil, fmt.Errorf("media input must be a regular file containing 1 to %d bytes", limit)
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || len(data) > limit {
		return nil, errors.New("media file changed size while reading")
	}
	return data, nil
}

func runMedia(s *workflow.Session, args []string, out map[string]interface{}) error {
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	to := flags.String("to", "", "exact recipient username")
	var file, thumb, md5 string
	var size, durationMS, seconds int64
	if args[0] == "emoji" {
		flags.StringVar(&md5, "md5", "", "existing sticker MD5")
		flags.Int64Var(&size, "size", 0, "existing sticker byte size")
	} else {
		flags.StringVar(&file, "file", "", "media file path")
	}
	if args[0] == "voice" {
		flags.Int64Var(&durationMS, "duration-ms", 0, "encoded audio duration in milliseconds, at most 60000")
	}
	if args[0] == "video" {
		flags.StringVar(&thumb, "thumb", "", "JPEG thumbnail path")
		flags.Int64Var(&seconds, "seconds", 0, "MP4 duration in seconds, at most 600")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected media arguments")
	}
	if *to == "" {
		return errors.New("an explicit recipient is required")
	}
	var receipt *workflow.MediaReceipt
	var err error
	switch args[0] {
	case "gif":
		var data []byte
		data, err = readMediaFile(file, workflow.MaxGIFBytes)
		if err == nil {
			receipt, err = s.SendGIF(*to, data)
		}
	case "image":
		var data []byte
		data, err = readMediaFile(file, workflow.MaxImageBytes)
		if err == nil {
			receipt, err = s.SendImage(*to, data)
		}
	case "emoji":
		if size <= 0 || size > 2147483647 {
			return errors.New("sticker size must be a positive int32 byte count")
		}
		receipt, err = s.SendEmoji(*to, md5, int32(size))
	case "voice":
		if durationMS < 1 || durationMS > 60000 {
			return errors.New("voice duration must be between 1 and 60000 milliseconds")
		}
		var data []byte
		data, err = readMediaFile(file, workflow.MaxVoiceBytes)
		if err == nil {
			receipt, err = s.SendVoice(*to, data, int32(durationMS))
		}
	case "video":
		if seconds < 1 || seconds > 600 {
			return errors.New("video duration must be between 1 and 600 seconds")
		}
		var video, thumbnail []byte
		video, err = readMediaFile(file, workflow.MaxVideoBytes)
		if err == nil {
			thumbnail, err = readMediaFile(thumb, workflow.MaxThumbnailBytes)
		}
		if err == nil {
			receipt, err = s.SendVideo(*to, video, thumbnail, uint32(seconds))
		}
	default:
		return errors.New("unknown media command")
	}
	if receipt != nil {
		out["receipt"] = receipt
	}
	return err
}
