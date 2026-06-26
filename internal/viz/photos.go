package viz

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/maxghenis/openmessage/internal/db"
)

const (
	maxPhotoBytes      = 15 << 20
	maxTotalPhotoBytes = 120 << 20
)

// Photo holds a photo's data URI and metadata for chronological placement.
type Photo struct {
	DataURI  string    `json:"data_uri"`
	Date     time.Time `json:"date"`
	Filename string    `json:"filename"`
}

// FindMediaMessages returns messages that have media attachments (images/videos).
func FindMediaMessages(messages []*db.Message) []*db.Message {
	var media []*db.Message
	for _, m := range messages {
		if m.MediaID != "" && isVisualMedia(m.MimeType) {
			media = append(media, m)
		}
	}
	return media
}

// isVisualMedia checks if a MIME type is an image or video.
func isVisualMedia(mime string) bool {
	return strings.HasPrefix(mime, "image/") || strings.HasPrefix(mime, "video/")
}

// EncodePhotosFromDir reads image files from a directory and returns them as
// Photos with base64 data URIs and parsed dates. Files are sorted by date
// (falling back to filename order). If maxPhotos > 0, evenly samples that
// many across the set.
func EncodePhotosFromDir(dir string, maxPhotos int) ([]Photo, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read dir: %w", err)
	}

	var imagePaths []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if e.Type()&os.ModeSymlink != 0 {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		switch ext {
		case ".jpg", ".jpeg", ".png", ".webp", ".gif":
			imagePaths = append(imagePaths, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(imagePaths)

	if maxPhotos > 0 && len(imagePaths) > maxPhotos {
		sampled := make([]string, maxPhotos)
		step := float64(len(imagePaths)) / float64(maxPhotos)
		for i := range maxPhotos {
			sampled[i] = imagePaths[int(float64(i)*step)]
		}
		imagePaths = sampled
	}

	return encodeFiles(imagePaths)
}

// EncodePhotosFromPaths encodes specific image files into Photos with dates.
// Used by the agent for curated photo selection.
func EncodePhotosFromPaths(paths []string) ([]Photo, error) {
	return encodeFiles(paths)
}

// encodeFiles reads a list of image file paths and returns Photos with
// base64 data URIs and dates parsed from filenames.
func encodeFiles(paths []string) ([]Photo, error) {
	var photos []Photo
	totalBytes := int64(0)
	for _, p := range paths {
		info, err := os.Lstat(p)
		if err != nil {
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if info.Size() > maxPhotoBytes {
			return nil, fmt.Errorf("%s exceeds %d bytes", p, maxPhotoBytes)
		}
		totalBytes += info.Size()
		if totalBytes > maxTotalPhotoBytes {
			return nil, fmt.Errorf("photos exceed %d bytes total", maxTotalPhotoBytes)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		mime := mimeFromExt(filepath.Ext(p))
		uri := fmt.Sprintf("data:%s;base64,%s", mime, base64.StdEncoding.EncodeToString(data))
		name := filepath.Base(p)
		photos = append(photos, Photo{
			DataURI:  uri,
			Date:     parseDateFromFilename(name),
			Filename: name,
		})
	}
	return photos, nil
}

// SortPhotosByDate sorts photos chronologically. Undated photos go to the end.
func SortPhotosByDate(photos []Photo) {
	sort.SliceStable(photos, func(i, j int) bool {
		di, dj := photos[i].Date, photos[j].Date
		if di.IsZero() && dj.IsZero() {
			return false
		}
		if di.IsZero() {
			return false
		}
		if dj.IsZero() {
			return true
		}
		return di.Before(dj)
	})
}

// parseDateFromFilename extracts a date from common photo filename patterns.
// Supports: IMG-20251211-WA0053.jpg (WhatsApp), IMG_20251211_123456.jpg,
// 20251211_123456.jpg, photo_2025-12-11.jpg, etc.
var datePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(\d{4})-(\d{2})-(\d{2})`), // 2025-12-11
	regexp.MustCompile(`(\d{4})(\d{2})(\d{2})`),   // 20251211
}

func parseDateFromFilename(name string) time.Time {
	for _, re := range datePatterns {
		if matches := re.FindStringSubmatch(name); len(matches) >= 4 {
			y, _ := strconv.Atoi(matches[1])
			m, _ := strconv.Atoi(matches[2])
			d, _ := strconv.Atoi(matches[3])
			if y >= 2000 && y <= 2100 && m >= 1 && m <= 12 && d >= 1 && d <= 31 {
				return time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC)
			}
		}
	}
	return time.Time{}
}

func mimeFromExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	default:
		return "image/jpeg"
	}
}
