// rpc_types.go
package main

import (
	"io/fs"
)

// DirEntryData 用於序列化 fs.DirEntry
type DirEntryData struct {
	Name  string
	IsDir bool
	Type  fs.FileMode
	Info  *FileInfoData // 可選，包含 Stat 信息
}

// FileInfoData 用於序列化 fs.FileInfo
type FileInfoData struct {
	Name    string
	Size    int64
	Mode    fs.FileMode
	ModTime int64 // 使用 int64 存儲時間戳（UnixNano）
	IsDir   bool
}

// 將 fs.DirEntry 轉換為 DirEntryData
func ToDirEntryData(entries []fs.DirEntry) ([]DirEntryData, error) {
	result := make([]DirEntryData, len(entries))
	for i, entry := range entries {
		info, err := entry.Info()
		var infoData *FileInfoData
		if err == nil {
			infoData = &FileInfoData{
				Name:    info.Name(),
				Size:    info.Size(),
				Mode:    info.Mode(),
				ModTime: info.ModTime().UnixNano(),
				IsDir:   info.IsDir(),
			}
		}
		// 可以選擇是否將 info.Err() 錯誤向上傳播
		result[i] = DirEntryData{
			Name:  entry.Name(),
			IsDir: entry.IsDir(),
			Type:  entry.Type(),
			Info:  infoData,
		}
	}
	return result, nil
}

// 將 fs.FileInfo 轉換為 FileInfoData
func ToFileInfoData(info fs.FileInfo) FileInfoData {
	return FileInfoData{
		Name:    info.Name(),
		Size:    info.Size(),
		Mode:    info.Mode(),
		ModTime: info.ModTime().UnixNano(),
		IsDir:   info.IsDir(),
	}
}
