package common

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"math/big"
	"math/rand"
	"mime"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/golang/glog"
	"github.com/jaypipes/ghw"
	"github.com/livepeer/go-livepeer/net"
	ffmpeg "github.com/livepeer/lpms/ffmpeg"
	"github.com/pkg/errors"
	"google.golang.org/grpc/peer"
)

// HTTPTimeout timeout used in HTTP connections between nodes
var HTTPTimeout = 8 * time.Second

// SegmentUploadTimeout timeout used in HTTP connections for uploading the segment
var SegmentUploadTimeout = 2 * time.Second

// Max Segment Duration
var MaxDuration = (5 * time.Minute)

// Max Input Bitrate (bits/sec)
var maxInputBitrate = 8000 * 1000 // 8000kbps

// Max Segment Size in bytes (cap reading HTTP response body at this size)
var MaxSegSize = int(MaxDuration.Seconds()) * (maxInputBitrate / 8)

const maxInt64 = int64(math.MaxInt64)

// using a scaleFactor of 1000 for orchestrator prices
// resulting in max decimal places of 3
const priceScalingFactor = int64(1000)

var (
	ErrParseBigInt = fmt.Errorf("failed to parse big integer")
	ErrProfile     = fmt.Errorf("failed to parse profile")

	ErrFormatProto = fmt.Errorf("unknown VideoProfile format for protobufs")
	ErrFormatMime  = fmt.Errorf("unknown VideoProfile format for mime type")
	ErrFormatExt   = fmt.Errorf("unknown VideoProfile format for extension")
	ErrProfProto   = fmt.Errorf("unknown VideoProfile profile for protobufs")
	ErrProfName    = fmt.Errorf("unknown VideoProfile profile name")

	ext2mime = map[string]string{
		".ts":  "video/mp2t",
		".mp4": "video/mp4",
	}
)

func init() {
	rand.Seed(time.Now().UnixNano())
}

func ParseBigInt(num string) (*big.Int, error) {
	bigNum := new(big.Int)
	_, ok := bigNum.SetString(num, 10)

	if !ok {
		return nil, ErrParseBigInt
	} else {
		return bigNum, nil
	}
}

func WaitUntil(waitTime time.Duration, condition func() bool) {
	start := time.Now()
	for time.Since(start) < waitTime {
		if condition() == false {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		break
	}
}

func WaitAssert(t *testing.T, waitTime time.Duration, condition func() bool, msg string) {
	start := time.Now()
	for time.Since(start) < waitTime {
		if condition() == false {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		break
	}

	if condition() == false {
		t.Errorf(msg)
	}
}

func Retry(attempts int, sleep time.Duration, fn func() error) error {
	if err := fn(); err != nil {
		if attempts--; attempts > 0 {
			time.Sleep(sleep)
			return Retry(attempts, 2*sleep, fn)
		}
		return err
	}

	return nil
}

func TxDataToVideoProfile(txData string) ([]ffmpeg.VideoProfile, error) {
	profiles := make([]ffmpeg.VideoProfile, 0)

	if len(txData) == 0 {
		return profiles, nil
	}
	if len(txData) < VideoProfileIDSize {
		return nil, ErrProfile
	}

	for i := 0; i+VideoProfileIDSize <= len(txData); i += VideoProfileIDSize {
		txp := txData[i : i+VideoProfileIDSize]

		p, ok := ffmpeg.VideoProfileLookup[VideoProfileNameLookup[txp]]
		if !ok {
			glog.Errorf("Cannot find video profile for job: %v", txp)
			return nil, ErrProfile // monitor to see if this is too aggressive
		}
		profiles = append(profiles, p)
	}

	return profiles, nil
}

func BytesToVideoProfile(txData []byte) ([]ffmpeg.VideoProfile, error) {
	profiles := make([]ffmpeg.VideoProfile, 0)

	if len(txData) == 0 {
		return profiles, nil
	}
	if len(txData) < VideoProfileIDBytes {
		return nil, ErrProfile
	}

	for i := 0; i+VideoProfileIDBytes <= len(txData); i += VideoProfileIDBytes {
		var txp [VideoProfileIDBytes]byte
		copy(txp[:], txData[i:i+VideoProfileIDBytes])

		p, ok := ffmpeg.VideoProfileLookup[VideoProfileByteLookup[txp]]
		if !ok {
			glog.Errorf("Cannot find video profile for job: %v", txp)
			return nil, ErrProfile // monitor to see if this is too aggressive
		}
		profiles = append(profiles, p)
	}

	return profiles, nil
}

func DefaultProfileName(width int, height int, bitrate int) string {
	return fmt.Sprintf("%dx%d_%d", width, height, bitrate)
}

func FFmpegProfiletoNetProfile(ffmpegProfiles []ffmpeg.VideoProfile) ([]*net.VideoProfile, error) {
	profiles := make([]*net.VideoProfile, 0, len(ffmpegProfiles))
	for _, profile := range ffmpegProfiles {
		width, height, err := ffmpeg.VideoProfileResolution(profile)
		if err != nil {
			return nil, err
		}
		br := strings.Replace(profile.Bitrate, "k", "000", 1)
		bitrate, err := strconv.Atoi(br)
		if err != nil {
			return nil, err
		}
		name := profile.Name
		if name == "" {
			name = "ffmpeg_" + DefaultProfileName(width, height, bitrate)
		}
		format := net.VideoProfile_MPEGTS
		switch profile.Format {
		case ffmpeg.FormatNone:
		case ffmpeg.FormatMPEGTS:
		case ffmpeg.FormatMP4:
			format = net.VideoProfile_MP4
		default:
			return nil, ErrFormatProto
		}
		encoderProf := net.VideoProfile_ENCODER_DEFAULT
		switch profile.Profile {
		case ffmpeg.ProfileNone:
		case ffmpeg.ProfileH264Baseline:
			encoderProf = net.VideoProfile_H264_BASELINE
		case ffmpeg.ProfileH264Main:
			encoderProf = net.VideoProfile_H264_MAIN
		case ffmpeg.ProfileH264High:
			encoderProf = net.VideoProfile_H264_HIGH
		case ffmpeg.ProfileH264ConstrainedHigh:
			encoderProf = net.VideoProfile_H264_CONSTRAINED_HIGH
		default:
			return nil, ErrProfProto
		}
		gop := int32(0)
		if profile.GOP < 0 {
			gop = int32(profile.GOP)
		} else {
			gop = int32(profile.GOP.Milliseconds())
		}
		fullProfile := net.VideoProfile{
			Name:    name,
			Width:   int32(width),
			Height:  int32(height),
			Bitrate: int32(bitrate),
			Fps:     uint32(profile.Framerate),
			FpsDen:  uint32(profile.FramerateDen),
			Format:  format,
			Profile: encoderProf,
			Gop:     gop,
		}
		profiles = append(profiles, &fullProfile)
	}
	return profiles, nil
}

func ProfilesToTranscodeOpts(profiles []ffmpeg.VideoProfile) []byte {
	transOpts := []byte{}
	for _, prof := range profiles {
		transOpts = append(transOpts, crypto.Keccak256([]byte(prof.Name))[0:4]...)
	}
	return transOpts
}

func ProfilesToHex(profiles []ffmpeg.VideoProfile) string {
	return hex.EncodeToString(ProfilesToTranscodeOpts(profiles))
}

func ProfilesNames(profiles []ffmpeg.VideoProfile) string {
	names := make(sort.StringSlice, 0, len(profiles))
	for _, p := range profiles {
		names = append(names, p.Name)
	}
	names.Sort()
	return strings.Join(names, ",")
}

func EncoderProfileNameToValue(profile string) (ffmpeg.Profile, error) {
	var EncoderProfileLookup = map[string]ffmpeg.Profile{
		"":                    ffmpeg.ProfileNone,
		"none":                ffmpeg.ProfileNone,
		"h264baseline":        ffmpeg.ProfileH264Baseline,
		"h264main":            ffmpeg.ProfileH264Main,
		"h264high":            ffmpeg.ProfileH264High,
		"h264constrainedhigh": ffmpeg.ProfileH264ConstrainedHigh,
	}
	p, ok := EncoderProfileLookup[strings.ToLower(profile)]
	if !ok {
		return -1, ErrProfName
	}
	return p, nil
}

func ProfileExtensionFormat(ext string) ffmpeg.Format {
	p, ok := ffmpeg.ExtensionFormats[ext]
	if !ok {
		return ffmpeg.FormatNone
	}
	return p
}

func ProfileFormatExtension(f ffmpeg.Format) (string, error) {
	ext, ok := ffmpeg.FormatExtensions[f]
	if !ok {
		return "", ErrFormatExt
	}
	return ext, nil
}

func ProfileFormatMimeType(f ffmpeg.Format) (string, error) {
	ext, err := ProfileFormatExtension(f)
	if err != nil {
		return "", err
	}
	return TypeByExtension(ext)
}

func TypeByExtension(ext string) (string, error) {
	if m, ok := ext2mime[ext]; ok && m != "" {
		return m, nil
	}
	m := mime.TypeByExtension(ext)
	if m == "" {
		return "", ErrFormatMime
	}
	return m, nil
}

func GetConnectionAddr(ctx context.Context) string {
	from := "unknown"
	if p, ok := peer.FromContext(ctx); ok {
		from = p.Addr.String()
	}
	return from
}

// GenErrRegex generates a regexp `(err1)|(err2)|(err3)` given a list of
// error strings [err1, err2, err3]
func GenErrRegex(errStrings []string) *regexp.Regexp {
	groups := []string{}
	for _, v := range errStrings {
		groups = append(groups, fmt.Sprintf("(%v)", v))
	}
	return regexp.MustCompile(strings.Join(groups, "|"))
}

// PriceToFixed converts a big.Rat into a fixed point number represented as int64
// using a scaleFactor of 1000 resulting in max decimal places of 3
func PriceToFixed(price *big.Rat) (int64, error) {
	return ratToFixed(price, priceScalingFactor)
}

// FixedToPrice converts an fixed point number with 3 decimal places represented as in int64 into a big.Rat
func FixedToPrice(price int64) *big.Rat {
	return big.NewRat(price, priceScalingFactor)
}

// BaseTokenAmountToFixed converts the base amount of a token (i.e. ETH/LPT) represented as a big.Int into a fixed point number represented
// as a int64 using a scalingFactor of 100000 resulting in max decimal places of 5
func BaseTokenAmountToFixed(baseAmount *big.Int) (int64, error) {
	// The base token amount is denominated in base units of a token
	// Assume that there are 10 ** 18 base units for each token
	// Convert the base token amount to a whole token amount by representing the base token amount
	// as a fraction with the denominator set to 10 ** 18
	var rat *big.Rat
	if baseAmount != nil {
		maxDecimals := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
		rat = new(big.Rat).SetFrac(baseAmount, maxDecimals)
	}

	return ratToFixed(rat, 100000)
}

func ratToFixed(rat *big.Rat, scalingFactor int64) (int64, error) {
	if rat == nil {
		return 0, fmt.Errorf("reference to rat is nil")
	}
	scaled := new(big.Rat).Mul(rat, big.NewRat(scalingFactor, 1))
	fp, _ := new(big.Float).SetRat(scaled).Int64()
	return fp, nil
}

// RandomIDGenerator generates random hexadecimal string of specified length
// defined as variable for unit tests
var RandomIDGenerator = func(length uint) string {
	return hex.EncodeToString(RandomBytesGenerator(length))
}

var RandomBytesGenerator = func(length uint) []byte {
	x := make([]byte, length, length)
	for i := 0; i < len(x); i++ {
		x[i] = byte(rand.Uint32())
	}
	return x
}

// RandName generates random hexadecimal string
func RandName() string {
	return RandomIDGenerator(10)
}

var RandomUintUnder = func(max uint) uint {
	return uint(rand.Uint32()) % max
}

func ToInt64(val *big.Int) int64 {
	if val.Cmp(big.NewInt(maxInt64)) > 0 {
		return maxInt64
	}

	return val.Int64()
}

func RatPriceInfo(priceInfo *net.PriceInfo) (*big.Rat, error) {
	if priceInfo == nil {
		return nil, nil
	}

	pixelsPerUnit := priceInfo.PixelsPerUnit
	if pixelsPerUnit == 0 {
		return nil, errors.New("pixels per unit is 0")
	}

	return big.NewRat(priceInfo.PricePerUnit, pixelsPerUnit), nil
}

func JoinURL(url, path string) string {
	if !strings.HasSuffix(url, "/") {
		return url + "/" + path
	}
	return url + path
}

// Read at most n bytes from an io.Reader
func ReadAtMost(r io.Reader, n int) ([]byte, error) {
	// Reading one extra byte to check if input Reader
	// had more than n bytes
	limitedReader := io.LimitReader(r, int64(n)+1)
	b, err := ioutil.ReadAll(limitedReader)
	if err == nil && len(b) > n {
		return nil, errors.New("input bigger than max buffer size")
	}
	return b, err
}

func detectNvidiaDevices() ([]string, error) {
	gpu, err := ghw.GPU()
	if err != nil {
		return nil, err
	}

	nvidiaCardCount := 0
	re := regexp.MustCompile("(?i)nvidia") // case insensitive match
	for _, card := range gpu.GraphicsCards {
		if re.MatchString(card.DeviceInfo.Vendor.Name) {
			nvidiaCardCount += 1
		}
	}
	if nvidiaCardCount == 0 {
		return nil, errors.New("no devices found with vendor name 'Nvidia'")
	}

	devices := []string{}

	for i := 0; i < nvidiaCardCount; i++ {
		s := strconv.Itoa(i)
		devices = append(devices, s)
	}

	return devices, nil
}

func ParseNvidiaDevices(nvidia string) ([]string, error) {
	if nvidia == "all" {
		return detectNvidiaDevices()
	}
	return strings.Split(nvidia, ","), nil
}
