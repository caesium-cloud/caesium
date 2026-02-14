package app

import "strings"

func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

func shortImage(image string) string {
	if image == "" {
		return "unknown"
	}
	if idx := strings.LastIndex(image, "/"); idx >= 0 && idx < len(image)-1 {
		image = image[idx+1:]
	}
	return image
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func mapContainsValue(m map[string]string, query string) bool {
	for k, v := range m {
		if strings.Contains(strings.ToLower(k), query) || strings.Contains(strings.ToLower(v), query) {
			return true
		}
	}
	return false
}
