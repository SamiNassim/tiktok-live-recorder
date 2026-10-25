package tiktok

import (
	"encoding/json"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

type pullData struct {
	Pull struct {
		Data string `json:"stream_data"`
	} `json:"pull_data"`
}
type user struct {
	RoomID   string `json:"roomId"`
	Username string `json:"uniqueId"`
	Nickname string `json:"nickname"`
	Avatar   string `json:"avatarLarger"`
}
type webResponse struct {
	Status  *int   `json:"statusCode"`
	Message string `json:"message"`
	Data    struct {
		User user `json:"user"`
		Room struct {
			Status *int     `json:"status"`
			Data   pullData `json:"streamData"`
			HEVC   pullData `json:"hevcStreamData"`
		} `json:"liveRoom"`
	} `json:"data"`
}

func (r webResponse) stream() (Stream, error) {
	if r.Message == "user_not_found" {
		return Stream{}, ErrNotFound
	}
	if r.Status == nil {
		return Stream{}, ErrBlocked
	}
	if *r.Status != 0 {
		return Stream{}, statusError(*r.Status)
	}
	s := Stream{RoomID: r.Data.User.RoomID, Name: r.Data.User.Nickname, Avatar: r.Data.User.Avatar}
	status := r.Data.Room.Status
	if status == nil {
		return s, ErrBlocked
	}
	if *status == 4 {
		return s, nil
	}
	if *status != 2 {
		return s, ErrBlocked
	}
	s.Live = true
	s.Sources = append(parseSDK(r.Data.Room.Data.Pull.Data), parseSDK(r.Data.Room.HEVC.Pull.Data)...)
	s.Sources = ordered(s.Sources)
	if len(s.Sources) == 0 {
		return s, ErrUnavailable
	}
	return s, nil
}

type roomResponse struct {
	Status *int `json:"status_code"`
	Data   struct {
		Status *int   `json:"status"`
		ID     string `json:"id_str"`
		Owner  struct {
			Name   string `json:"nickname"`
			Avatar struct {
				URLs []string `json:"url_list"`
			} `json:"avatar_thumb"`
		} `json:"owner"`
		Prompts json.RawMessage `json:"prompts"`
		URL     struct {
			SDK  pullData          `json:"live_core_sdk_data"`
			FLV  map[string]string `json:"flv_pull_url"`
			Pull string            `json:"rtmp_pull_url"`
		} `json:"stream_url"`
	} `json:"data"`
}

func (r roomResponse) stream() (Stream, error) {
	if r.Status == nil {
		return Stream{}, ErrBlocked
	}
	if *r.Status != 0 {
		return Stream{}, statusError(*r.Status)
	}
	s := Stream{RoomID: r.Data.ID, Name: r.Data.Owner.Name}
	if len(r.Data.Owner.Avatar.URLs) > 0 {
		s.Avatar = r.Data.Owner.Avatar.URLs[0]
	}
	if r.Data.Status == nil {
		if len(r.Data.Prompts) > 0 {
			return s, ErrAccess
		}
		return s, ErrBlocked
	}
	if *r.Data.Status == 4 {
		return s, nil
	}
	if *r.Data.Status != 2 {
		return s, ErrBlocked
	}
	s.Live = true
	s.Sources = parseSDK(r.Data.URL.SDK.Pull.Data)
	for quality, address := range r.Data.URL.FLV {
		if validMediaURL(address) {
			s.Sources = append(s.Sources, Source{URL: address, Quality: strings.ToLower(quality)})
		}
	}
	if validMediaURL(r.Data.URL.Pull) {
		s.Sources = append(s.Sources, Source{URL: r.Data.URL.Pull, Quality: "origin"})
	}
	s.Sources = ordered(s.Sources)
	if len(s.Sources) == 0 {
		if len(r.Data.Prompts) > 0 {
			return s, ErrAccess
		}
		return s, ErrUnavailable
	}
	return s, nil
}

func statusError(code int) error {
	switch code {
	case 19881007, 10202:
		return ErrNotFound
	case 4003110, 20003, 8:
		return ErrAccess
	default:
		return ErrBlocked
	}
}

func validMediaURL(address string) bool {
	u, err := url.Parse(address)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && strings.HasSuffix(strings.ToLower(u.Path), ".flv")
}

func parseSDK(raw string) []Source {
	var data struct {
		Data map[string]struct {
			Main struct {
				FLV    string `json:"flv"`
				Params string `json:"sdk_params"`
			} `json:"main"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(raw), &data) != nil {
		return nil
	}
	var sources []Source
	for quality, entry := range data.Data {
		if quality == "ao" || !validMediaURL(entry.Main.FLV) {
			continue
		}
		s := Source{URL: entry.Main.FLV, Quality: quality}
		var params struct {
			Codec      string `json:"VCodec"`
			LowerCodec string `json:"v_codec"`
			Resolution string `json:"resolution"`
			Bitrate    int    `json:"vbitrate"`
		}
		if json.Unmarshal([]byte(entry.Main.Params), &params) == nil {
			s.Codec, s.Bitrate = params.Codec, params.Bitrate
			if s.Codec == "" {
				s.Codec = params.LowerCodec
			}
			parts := strings.Split(strings.ToLower(params.Resolution), "x")
			if len(parts) == 2 {
				s.Width, _ = strconv.Atoi(parts[0])
				s.Height, _ = strconv.Atoi(parts[1])
			}
		}
		sources = append(sources, s)
	}
	return sources
}

func ordered(sources []Source) []Source {
	// Never prefer an adaptive/audio-only rendition over an explicitly offered
	// source stream. Preserve the original codec instead of re-encoding it.
	rank := func(s Source) int {
		switch strings.ToLower(s.Quality) {
		case "origin", "origion":
			return 100
		case "uhd_60":
			return 90
		case "uhd", "full_hd1":
			return 80
		case "hd_60":
			return 70
		case "hd", "hd1":
			return 60
		case "sd", "sd2":
			return 40
		case "ld", "sd1":
			return 20
		default:
			return 0
		}
	}
	slices.SortStableFunc(sources, func(a, b Source) int {
		if n := rank(b) - rank(a); n != 0 {
			return n
		}
		if n := b.Width*b.Height - a.Width*a.Height; n != 0 {
			return n
		}
		if n := b.Bitrate - a.Bitrate; n != 0 {
			return n
		}
		return strings.Compare(a.URL, b.URL)
	})
	seen := make(map[string]bool)
	return slices.DeleteFunc(sources, func(s Source) bool {
		if seen[s.URL] {
			return true
		}
		seen[s.URL] = true
		return false
	})
}
