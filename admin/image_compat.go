package admin

import (
	"os"
	"strings"
)

// 这些小工具原先位于已删除的 image_studio.go。Image Studio 功能（上游生图）
// 已随 qoder 化移除，但 imageAssetDir / firstNonEmpty 仍被 bootstrap、
// runtime_status、handler 的非图片代码路径引用，故保留。

const defaultImageAssetDir = "/data/images"

func imageAssetDir() string {
	if dir := strings.TrimSpace(os.Getenv("IMAGE_ASSET_DIR")); dir != "" {
		return dir
	}
	return defaultImageAssetDir
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
