package media

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Formats is the demuxer whitelist for every probe and conversion: the
// containers phones, screen recorders and sound apps write. Nothing else
// is opened — an HLS or concat playlist dressed as a video is the known
// way to make ffmpeg read other local files — and file is the only
// protocol.
const Formats = "mov,matroska,ogg,mp3,wav,flac,aac,gif"

// Kinds of result.
const (
	Video   = "video"   // H.264 + AAC mp4 with a JPEG poster
	Audio   = "audio"   // AAC m4a with a PNG waveform as its poster
	Loop    = "loop"    // an animated GIF as a silent mp4 that loops
	Picture = "picture" // a still GIF, kept as it came
)

// Converter runs ffmpeg for one hub. The zero values of the limits are
// replaced by New's defaults.
type Converter struct {
	FFmpeg, FFprobe string
	// NVENC encodes H.264 on the GPU; Vulkan decodes H.264 and HEVC,
	// scales and tone-maps there (libplacebo). Either falls back
	// to the CPU: libx264, swscale and zscale.
	NVENC, Vulkan bool
	MaxVideo      time.Duration // longest video or animation accepted
	MaxAudio      time.Duration // longest sound accepted
	Cap           int64         // an output file's size limit, under the hub's 8 MB embed cap
	Log           func(format string, args ...any)
}

// Encoder names what encodes the video, for /v1/hub and the log.
func (c *Converter) Encoder() string {
	switch {
	case c.NVENC && c.Vulkan:
		return "nvenc+vulkan"
	case c.NVENC:
		return "nvenc"
	}
	return "x264"
}

// New finds ffmpeg and ffprobe and settles the encoder: "auto" tries
// NVENC and the Vulkan filters with a tiny test run of each, "nvenc"
// requires NVENC, "x264" uses the CPU alone.
func New(ffmpeg, ffprobe, encoder string) (*Converter, error) {
	c := &Converter{MaxVideo: 3 * time.Minute, MaxAudio: 10 * time.Minute, Cap: 7_600_000, Log: func(string, ...any) {}}
	var err error
	if c.FFmpeg, err = exec.LookPath(or(ffmpeg, "ffmpeg")); err != nil {
		return nil, err
	}
	if c.FFprobe, err = exec.LookPath(or(ffprobe, "ffprobe")); err != nil {
		return nil, err
	}
	switch encoder {
	case "", "auto", "nvenc":
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c.NVENC = c.run(ctx, nil, "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=30", "-frames:v", "3",
			"-c:v", "h264_nvenc", "-f", "null", "-") == nil
		c.Vulkan = c.run(ctx, nil, "-init_hw_device", "vulkan=vk", "-filter_hw_device", "vk",
			"-f", "lavfi", "-i", "testsrc2=size=64x48", "-frames:v", "1",
			"-vf", "format=yuv420p,hwupload,libplacebo=w=48:h=64:format=yuv420p,hwdownload,format=yuv420p",
			"-f", "null", "-") == nil
		if encoder == "nvenc" && !c.NVENC {
			return nil, errors.New(`media.encoder "nvenc": h264_nvenc does not run here`)
		}
	case "x264":
	default:
		return nil, fmt.Errorf("media.encoder %q (want auto, nvenc or x264)", encoder)
	}
	return c, nil
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// Probe is what ffprobe says about an input.
type Probe struct {
	Format   string
	Duration float64 // seconds
	Video    *Stream // the first real video stream; cover art is not one
	Audio    bool
	Frames   int // packets in the video stream, counted for GIFs only
}

type Stream struct {
	Codec    string
	Width    int // as stored
	Height   int
	Rotation int  // degrees the picture turns to display: 0, 90, 180 or 270
	HDR      bool // PQ or HLG
	FPS      float64
}

// Shown is the stream's size as displayed, rotation applied.
func (s *Stream) Shown() (int, int) {
	if s.Rotation == 90 || s.Rotation == 270 {
		return s.Height, s.Width
	}
	return s.Width, s.Height
}

// Probe reads an input through the whitelists.
func (c *Converter) Probe(ctx context.Context, path string) (*Probe, error) {
	out, err := c.probeJSON(ctx, "-show_format", "-show_streams", path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Format struct {
			Name     string `json:"format_name"`
			Duration string `json:"duration"`
		} `json:"format"`
		Streams []struct {
			Type        string `json:"codec_type"`
			Codec       string `json:"codec_name"`
			Width       int    `json:"width"`
			Height      int    `json:"height"`
			Transfer    string `json:"color_transfer"`
			Rate        string `json:"avg_frame_rate"`
			Duration    string `json:"duration"`
			Disposition struct {
				AttachedPic int `json:"attached_pic"`
			} `json:"disposition"`
			SideData []struct {
				Rotation *float64 `json:"rotation"`
			} `json:"side_data_list"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("ffprobe: %w", err)
	}
	p := &Probe{Format: doc.Format.Name}
	p.Duration, _ = strconv.ParseFloat(doc.Format.Duration, 64)
	for _, st := range doc.Streams {
		switch {
		case st.Type == "audio":
			p.Audio = true
		case st.Type == "video" && st.Disposition.AttachedPic == 0 && p.Video == nil:
			v := &Stream{Codec: st.Codec, Width: st.Width, Height: st.Height,
				HDR: st.Transfer == "smpte2084" || st.Transfer == "arib-std-b67"}
			for _, sd := range st.SideData {
				if sd.Rotation != nil {
					// ffprobe's rotation turns the picture counter-clockwise;
					// what is shown is the picture turned the other way
					v.Rotation = ((-int(math.Round(*sd.Rotation))%360 + 360) % 360)
				}
			}
			if n, d, ok := strings.Cut(st.Rate, "/"); ok {
				nf, _ := strconv.ParseFloat(n, 64)
				df, _ := strconv.ParseFloat(d, 64)
				if df > 0 {
					v.FPS = nf / df
				}
			}
			if p.Duration == 0 {
				p.Duration, _ = strconv.ParseFloat(st.Duration, 64)
			}
			p.Video = v
		}
	}
	if strings.Contains(p.Format, "gif") && p.Video != nil {
		out, err := c.probeJSON(ctx, "-count_packets", "-select_streams", "v:0", "-show_entries", "stream=nb_read_packets", path)
		if err != nil {
			return nil, err
		}
		var n struct {
			Streams []struct {
				Packets string `json:"nb_read_packets"`
			} `json:"streams"`
		}
		if json.Unmarshal(out, &n) == nil && len(n.Streams) > 0 {
			p.Frames, _ = strconv.Atoi(n.Streams[0].Packets)
		}
	}
	return p, nil
}

func (c *Converter) probeJSON(ctx context.Context, args ...string) ([]byte, error) {
	args = append([]string{"-v", "error", "-protocol_whitelist", "file", "-format_whitelist", Formats, "-of", "json"}, args...)
	var stdout, stderr bytes.Buffer
	cmd := command(ctx, c.FFprobe, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrUnreadable, lastLine(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// ErrUnreadable is a file ffprobe cannot open through the whitelists: not
// media, or media of a kind the hub does not take.
var ErrUnreadable = errors.New("not a video, sound or GIF this hub can read")

// Result is one conversion's output, in the job's directory.
type Result struct {
	Kind       string
	File, MIME string
	Poster     string // path; "" for a picture
	PosterMIME string
	Width      int
	Height     int
	Duration   float64
}

// Convert turns the input into its kind's output in dir. progress, when
// set, hears the fraction done now and then.
func (c *Converter) Convert(ctx context.Context, in, dir string, progress func(float64)) (*Result, error) {
	p, err := c.Probe(ctx, in)
	if err != nil {
		return nil, err
	}
	if progress == nil {
		progress = func(float64) {}
	}
	switch {
	case strings.Contains(p.Format, "gif") && p.Video != nil:
		if p.Frames <= 1 {
			st, err := os.Stat(in)
			if err != nil {
				return nil, err
			}
			if st.Size() > c.Cap {
				return nil, errors.New("a still GIF over 8 MB: save it as a PNG or JPEG")
			}
			w, h := p.Video.Shown()
			return &Result{Kind: Picture, File: in, MIME: "image/gif", Width: w, Height: h}, nil
		}
		return c.video(ctx, in, dir, p, true, progress)
	case p.Video != nil:
		return c.video(ctx, in, dir, p, false, progress)
	case p.Audio:
		return c.audio(ctx, in, dir, p, progress)
	}
	return nil, errors.New("no picture or sound in this file")
}

// Tier is the largest frame a video of a given length gets, so its
// bitrate under the cap stays watchable: 1080p up to 30 s, 720p up to a
// minute, 480p beyond.
func Tier(d float64) (long, short, maxBPS int) {
	switch {
	case d <= 30:
		return 1920, 1080, 8_000_000
	case d <= 60:
		return 1280, 720, 4_000_000
	}
	return 854, 480, 2_000_000
}

// Fit scales w×h into the tier's box without enlarging it, to even sides.
func Fit(w, h, long, short int) (int, int) {
	f := math.Min(1, math.Min(float64(long)/float64(max(w, h)), float64(short)/float64(min(w, h))))
	even := func(v float64) int { return max(2, int(math.Round(v/2))*2) }
	return even(float64(w) * f), even(float64(h) * f)
}

// budget is the bits a second the cap allows over d seconds, less the
// mp4's own overhead.
func (c *Converter) budget(d float64) int {
	return int(float64(c.Cap) * 8 * 0.94 / math.Max(d, 1))
}

func (c *Converter) video(ctx context.Context, in, dir string, p *Probe, loop bool, progress func(float64)) (*Result, error) {
	v := p.Video
	run := func(watch func(io.Reader), args ...string) error {
		ctx, cancel := limit(ctx, p.Duration)
		defer cancel()
		return c.run(ctx, watch, args...)
	}
	if p.Duration <= 0 {
		return nil, errors.New("this video does not say how long it is")
	}
	if p.Duration > c.MaxVideo.Seconds() {
		return nil, fmt.Errorf("videos are capped at %s; this one runs %s", minutes(c.MaxVideo.Seconds()), minutes(p.Duration))
	}
	if v.Width <= 0 || v.Height <= 0 || v.Width*v.Height > 8192*8192 {
		return nil, fmt.Errorf("a %d×%d picture is out of range", v.Width, v.Height)
	}
	long, short, maxBPS := Tier(p.Duration)
	sw, sh := v.Shown()
	w, h := Fit(sw, sh, long, short)
	audio := p.Audio && !loop
	abps := 0
	if audio {
		abps = 128_000
		if p.Duration > 60 {
			abps = 96_000
		}
	}
	vbps := min(maxBPS, c.budget(p.Duration)-abps)
	if vbps < 100_000 {
		return nil, errors.New("too long to fit 8 MB")
	}
	fps := 60
	if p.Duration > 30 || loop {
		fps = 30
	}
	out := filepath.Join(dir, "out.mp4")
	// the GPU chain decodes what phones record; anything else, or a GPU
	// run that fails, goes through the CPU
	gpu := c.Vulkan && !loop && (v.Codec == "h264" || v.Codec == "hevc")
	nvenc := c.NVENC
	for try := 0; ; try++ {
		args := c.videoArgs(in, out, v, w, h, vbps, abps, fps, gpu, nvenc)
		err := run(c.progress(p.Duration, progress), args...)
		if err != nil && gpu {
			c.Log("media: GPU chain failed (%v); converting on the CPU", err)
			gpu = false
			err = run(c.progress(p.Duration, progress), c.videoArgs(in, out, v, w, h, vbps, abps, fps, false, nvenc)...)
		}
		if err != nil {
			return nil, err
		}
		st, err := os.Stat(out)
		if err != nil {
			return nil, err
		}
		if st.Size() <= c.Cap {
			break
		}
		if try == 3 {
			return nil, errors.New("could not fit this video in 8 MB")
		}
		// overshot: aim under by the ratio it missed by, and a margin.
		// NVENC has a floor it will not go under on grainy video (even
		// QP 51 writes megabytes of noise), so a second miss goes to x264
		vbps = int(float64(vbps) * float64(c.Cap) / float64(st.Size()) * 0.9)
		if try == 1 && nvenc {
			nvenc = false
			vbps = min(maxBPS, c.budget(p.Duration)-abps)
		}
		c.Log("media: %d bytes over a %d cap; again at %d b/s (nvenc=%v)", st.Size(), c.Cap, vbps, nvenc)
	}
	poster := filepath.Join(dir, "poster.jpg")
	pf := "thumbnail=n=48"
	if loop {
		pf = "null" // an animation starts where it starts
	}
	if err := run(nil, "-protocol_whitelist", "file", "-i", out, "-vf", pf, "-frames:v", "1", "-q:v", "3", poster); err != nil {
		return nil, err
	}
	// the length of what was written, which is what a player shows
	d := p.Duration
	if op, err := c.Probe(ctx, out); err == nil && op.Duration > 0 {
		d = op.Duration
	}
	kind := Video
	if loop {
		kind = Loop
	}
	return &Result{Kind: kind, File: out, MIME: "video/mp4", Poster: poster, PosterMIME: "image/jpeg",
		Width: w, Height: h, Duration: math.Round(d*1000) / 1000}, nil
}

func (c *Converter) videoArgs(in, out string, v *Stream, w, h, vbps, abps, fps int, gpu, nvenc bool) []string {
	var args []string
	var vf []string
	if gpu {
		// decoded, scaled and tone-mapped in Vulkan, then turned on the
		// CPU at the output size. ffmpeg's autorotate skips GPU frames (a
		// portrait phone video came out sideways) but still drops the
		// display matrix, so the turn is done here and nothing turns it
		// twice; transpose_vulkan left a green row along one edge
		args = append(args, "-init_hw_device", "vulkan=vk", "-hwaccel", "vulkan", "-hwaccel_output_format", "vulkan",
			"-hwaccel_device", "vk", "-filter_hw_device", "vk")
		sw, sh := w, h
		if v.Rotation == 90 || v.Rotation == 270 {
			sw, sh = h, w
		}
		vf = append(vf, fmt.Sprintf("libplacebo=w=%d:h=%d:format=yuv420p:colorspace=bt709:color_primaries=bt709:color_trc=bt709:range=tv:tonemapping=auto", sw, sh),
			"hwdownload", "format=yuv420p")
		switch v.Rotation {
		case 90:
			vf = append(vf, "transpose=clock")
		case 180:
			vf = append(vf, "hflip", "vflip")
		case 270:
			vf = append(vf, "transpose=cclock")
		}
	} else {
		// autorotate turns the frames before this chain
		if v.HDR {
			vf = append(vf, fmt.Sprintf("scale=%d:%d", w, h),
				"zscale=t=linear:npl=100", "format=gbrpf32le", "zscale=p=bt709",
				"tonemap=tonemap=hable:desat=0", "zscale=t=bt709:m=bt709:r=tv")
		} else {
			vf = append(vf, fmt.Sprintf("scale=%d:%d:flags=bicubic", w, h))
		}
		vf = append(vf, "setsar=1", "format=yuv420p")
	}
	args = append(args, "-protocol_whitelist", "file", "-format_whitelist", Formats, "-i", in,
		"-map", "0:v:0", "-vf", strings.Join(vf, ","), "-fpsmax", strconv.Itoa(fps))
	// a quality target under a ceiling: an easy picture (a dark beach at
	// night) comes out a few megabytes, and only a hard one reaches the
	// rate that fills the cap. On an iPhone's 12 s 4K clip, filling the
	// cap wrote 7.25 MB at SSIM 0.9980 and cq 26 wrote 2.5 MB at 0.9969
	ceiling := []string{"-maxrate", strconv.Itoa(vbps), "-bufsize", strconv.Itoa(vbps * 2)}
	if nvenc {
		args = append(args, "-c:v", "h264_nvenc", "-preset", "p5", "-tune", "hq", "-profile:v", "high", "-rc", "vbr", "-cq", "25", "-b:v", "0")
	} else {
		args = append(args, "-c:v", "libx264", "-preset", "veryfast", "-profile:v", "high", "-crf", "22")
	}
	args = append(args, ceiling...)
	if abps > 0 {
		args = append(args, "-map", "0:a:0", "-c:a", "aac", "-b:a", strconv.Itoa(abps), "-ac", "2")
	} else {
		args = append(args, "-an")
	}
	// the phone's GPS, the camera's name, chapters, subtitles and data
	// tracks stay behind
	return append(args, "-map_metadata", "-1", "-map_metadata:s:v", "-1", "-map_metadata:s:a", "-1",
		"-map_chapters", "-1", "-sn", "-dn", "-movflags", "+faststart", "-f", "mp4", out)
}

func (c *Converter) audio(ctx context.Context, in, dir string, p *Probe, progress func(float64)) (*Result, error) {
	if p.Duration <= 0 {
		return nil, errors.New("this sound does not say how long it is")
	}
	if p.Duration > c.MaxAudio.Seconds() {
		return nil, fmt.Errorf("sounds are capped at %s; this one runs %s", minutes(c.MaxAudio.Seconds()), minutes(p.Duration))
	}
	run := func(watch func(io.Reader), args ...string) error {
		ctx, cancel := limit(ctx, p.Duration)
		defer cancel()
		return c.run(ctx, watch, args...)
	}
	abps := min(160_000, c.budget(p.Duration))
	out := filepath.Join(dir, "out.m4a")
	if err := run(c.progress(p.Duration, progress), "-protocol_whitelist", "file", "-format_whitelist", Formats, "-i", in,
		"-map", "0:a:0", "-vn", "-c:a", "aac", "-b:a", strconv.Itoa(abps), "-ac", "2",
		"-map_metadata", "-1", "-map_metadata:s:a", "-1", "-map_chapters", "-1",
		"-movflags", "+faststart", "-f", "ipod", out); err != nil {
		return nil, err
	}
	// the waveform, dark on clear, drawn at twice the size it is shown
	wave := filepath.Join(dir, "wave.png")
	if err := run(nil, "-protocol_whitelist", "file", "-i", out, "-filter_complex",
		"aformat=channel_layouts=mono,showwavespic=s=1200x96:colors=0x262626:scale=sqrt:draw=full",
		"-frames:v", "1", wave); err != nil {
		return nil, err
	}
	d := p.Duration
	if op, err := c.Probe(ctx, out); err == nil && op.Duration > 0 {
		d = op.Duration
	}
	return &Result{Kind: Audio, File: out, MIME: "audio/mp4", Poster: wave, PosterMIME: "image/png",
		Width: 1200, Height: 96, Duration: math.Round(d*1000) / 1000}, nil
}

// limit bounds one ffmpeg run over d seconds of media: twice its length
// and 30 s (the Vulkan device alone takes a few), then the run is killed
// with its process group.
func limit(ctx context.Context, d float64) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, time.Duration((2*d+30)*float64(time.Second)))
}

// progress reads ffmpeg's -progress lines into a fraction of d seconds.
func (c *Converter) progress(d float64, report func(float64)) func(io.Reader) {
	return func(r io.Reader) {
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			if us, ok := strings.CutPrefix(sc.Text(), "out_time_us="); ok {
				if n, err := strconv.ParseInt(us, 10, 64); err == nil && d > 0 {
					report(math.Min(1, float64(n)/1e6/d))
				}
			}
		}
	}
}

// run is one ffmpeg in its own process group, niced, killed with the group
// when ctx ends; stdout (the -progress stream) goes to watch.
func (c *Converter) run(ctx context.Context, watch func(io.Reader), args ...string) error {
	head := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y"}
	if watch != nil {
		head = append(head, "-progress", "pipe:1", "-nostats")
	}
	var stderr bytes.Buffer
	cmd := command(ctx, c.FFmpeg, append(head, args...)...)
	cmd.Stderr = &stderr
	if watch != nil {
		pr, pw := io.Pipe()
		cmd.Stdout = pw
		done := make(chan struct{})
		go func() { watch(pr); io.Copy(io.Discard, pr); close(done) }()
		defer func() { pw.Close(); <-done }()
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	syscall.Setpriority(syscall.PRIO_PROCESS, cmd.Process.Pid, 10)
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("ffmpeg stopped: %w", ctx.Err())
		}
		return fmt.Errorf("ffmpeg: %s", or(lastLine(stderr.String()), err.Error()))
	}
	return nil
}

func command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

// lastLine is the last line of ffmpeg's errors that says something: the
// Vulkan loader's complaints about other drivers are noise.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l != "" && !strings.HasPrefix(l, "MESA:") && !strings.HasPrefix(l, "TU:") {
			return l
		}
	}
	return ""
}

func minutes(s float64) string {
	sec := int(math.Round(s))
	return fmt.Sprintf("%d:%02d", sec/60, sec%60)
}
