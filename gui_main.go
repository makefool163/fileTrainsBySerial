package main

import (
	"embed"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"
	"go.bug.st/serial"
)

type FileManagerApp struct {
	window             fyne.Window
	statusLabel        *widget.Label
	progressBar        *widget.ProgressBar // 添加进度条
	selectedRemoteFile *FileItem           // 添加选中的远程文件

	// 本地文件管理器组件
	localBreadcrumb  *widget.List
	localFileGrid    *widget.List
	localCurrentPath string
	localPathStack   []string

	// 远程文件管理器组件
	remoteBreadcrumb  *widget.List
	remoteFileGrid    *widget.List
	remoteCurrentPath string
	remotePathStack   []string

	// RPC 节点
	rpcNode                *SerialRPCNode
	last_remoteCurrentPath string
	files                  []FileItem
}

type FileItem struct {
	Name    string
	IsDir   bool
	Size    int64
	ModTime time.Time
	Path    string // 添加完整路径字段
}

func (fm *FileManagerApp) initializePaths() {
	if runtime.GOOS == "windows" {
		fm.localCurrentPath = "\\"
		fm.localPathStack = []string{"\\"}
	} else {
		fm.localCurrentPath = "/"
		fm.localPathStack = []string{"/"}
	}

	// 远程路径初始化
	fm.remoteCurrentPath = "/"
	fm.remotePathStack = []string{"/"}

	// 初始化进度条
	fm.progressBar = widget.NewProgressBar()
	fm.progressBar.Hide() // 默认隐藏进度条
}

func (fm *FileManagerApp) createUI() *fyne.Container {
	// 创建顶部工具条
	toolbar := fm.createToolbar()

	// 创建中间容器（左右分割）
	mainContainer := fm.createMainContainer()

	// 创建底部状态条（包含进度条）
	statusContainer := container.NewVBox(
		fm.progressBar,
		container.NewBorder(nil, nil, nil, nil, fm.statusLabel),
	)

	// 组合整体布局
	return container.NewBorder(toolbar, statusContainer, nil, nil, mainContainer)
}

func (fm *FileManagerApp) createToolbar() *fyne.Container {
	// 串口配置
	ports, _ := serial.GetPortsList()
	portLabel := widget.NewLabel("端口")
	portCombo := widget.NewSelect(ports, nil)
	// portCombo := widget.NewSelect([]string{"COM1", "COM2", "COM3", "COM4"}, nil)
	portCombo.SetSelected(ports[0])

	baudrateLabel := widget.NewLabel("波特率")
	baudrateCombo := widget.NewSelect([]string{"9600", "115200", "460800", "921600", "2000000"}, nil)
	baudrateCombo.SetSelected("115200")

	// flowControlLabel := widget.NewLabel("流控制")
	// flowControlCombo := widget.NewSelect([]string{"None", "硬件流控制", "软件流控制"}, nil)
	// flowControlCombo.SetSelected("None")

	// dataBitsLabel := widget.NewLabel("数据位")
	// dataBitsCombo := widget.NewSelect([]string{"5", "6", "7", "8"}, nil)
	// dataBitsCombo.SetSelected("8")

	// parityLabel := widget.NewLabel("校验位")
	// parityCombo := widget.NewSelect([]string{"无校验", "奇校验", "偶校验"}, nil)
	// parityCombo.SetSelected("无校验")

	// 刷新远程按钮
	refreshRemoteBtn := widget.NewButton("刷新远程", func() {
		fm.last_remoteCurrentPath = "" // 清空缓存，强制刷新
		fm.refreshRemoteFiles()
	})

	downloadBtn := widget.NewButton("下载文件", func() {
		fm.downloadFile()
		// fm.statusLabel.SetText("下载功能暂未实现")
	})

	// uploadBtn := widget.NewButton("上传文件", func() {
	// 	fm.statusLabel.SetText("上传功能暂未实现")
	// })

	var connectBtn *widget.Button
	var disconnectBtn *widget.Button

	disconnectBtn = widget.NewButton("断开", func() {
		// fm.rpcNode.Stop()
		fm.rpcNode.Close()
		fyne.Do(func() {
			connectBtn.Enable()
			downloadBtn.Disable()
			disconnectBtn.Disable()
			refreshRemoteBtn.Disable()

		})
		fm.statusLabel.SetText("已断开连接")
	})

	// 操作按钮
	connectBtn = widget.NewButton("连接", func() {
		fm.statusLabel.SetText("正在连接...")
		baudrate, err := strconv.Atoi(baudrateCombo.Selected)
		if err != nil {
			fm.statusLabel.SetText("波特率无效")
			log.Printf("无效波特率: %v", err)
			return
		}
		fm.rpcNode, err = NewSerialRPCNode(portCombo.Selected, baudrate, 0)
		if err != nil {
			fmt.Printf("打開串口 失敗: %v\n", err)
			return
		}
		// defer fm.rpcNode.Close()

		go fm.rpcNode.Start()
		fm.rpcNode.WaitForStart()

		// 注册函数到 RPC 节点
		if err := fm.rpcNode.Register("ReadDir", os.ReadDir); err != nil {
			fmt.Printf("节点B注册 ReadDir 失败: %v\n", err)
			return
		}
		if err := fm.rpcNode.Register("Stat", os.Stat); err != nil {
			fmt.Printf("节点B注册 Stat 失败: %v\n", err)
			return
		}
		// 添加这行注册新的GetFileSplit函数
		if err := fm.rpcNode.Register("GetFileSplit", GetFileSplit); err != nil {
			fmt.Printf("节点B注册 GetFileSplit 失败: %v\n", err)
			return
		}
		if err := fm.rpcNode.Register("OsIsWindows", OsIsWindows); err != nil {
			fmt.Printf("节点B注册 OsIsWindows 失败: %v\n", err)
			return
		}

		fyne.Do(func() {
			connectBtn.Disable()
			downloadBtn.Enable()
			disconnectBtn.Enable()
			refreshRemoteBtn.Enable()

		})
		fm.statusLabel.SetText("串口已打開，等待操作...")
		// fm.refreshRemoteFiles()
	})

	downloadBtn.Disable()
	disconnectBtn.Disable()
	refreshRemoteBtn.Disable()
	return container.NewHBox(
		portLabel, portCombo,
		baudrateLabel, baudrateCombo,
		// flowControlLabel, flowControlCombo,
		// dataBitsLabel, dataBitsCombo,
		// parityLabel, parityCombo,
		connectBtn, disconnectBtn,
		refreshRemoteBtn,
		// uploadBtn,
		downloadBtn,
	)
}

// 执行文件下载的函数
func (fm *FileManagerApp) performDownload(remoteFilename, localPath string, filesize int64) error {
	const chunkSize int64 = 4 * 1024 // 每次下载的分片大小

	// 创建本地文件
	localFile, err := os.Create(localPath)
	if err != nil {
		fm.updateStatus(fmt.Sprintf("创建本地文件失败: %v", err))
		fm.hideProgressBar()
		return err
	}
	defer localFile.Close()

	// 分片下载文件
	var downloaded int64 = 0
	for downloaded < filesize {
		// 计算本次需要读取的大小
		remaining := filesize - downloaded
		currentChunkSize := chunkSize
		if remaining < chunkSize {
			currentChunkSize = remaining
		}

		// 调用远程函数获取文件分片
		result, err := fm.rpcNode.Call("GetFileSplit", remoteFilename, downloaded, currentChunkSize)
		if err != nil {
			fm.updateStatus(fmt.Sprintf("获取文件分片失败: %v", err))
			fm.hideProgressBar()
			return err
		}

		// 写入本地文件
		data := result[0].([]byte)
		_, err = localFile.Write(data)
		if err != nil {
			fm.updateStatus(fmt.Sprintf("写入本地文件失败: %v", err))
			fm.hideProgressBar()
			return err
		}

		// 更新下载进度
		downloaded += int64(len(data))
		progress := float64(downloaded) / float64(filesize)

		// 更新进度条
		fm.updateProgress(progress, remoteFilename)
	}

	// 下载完成
	fm.updateStatus(fmt.Sprintf("文件下载完成: %s", remoteFilename))
	fm.hideProgressBar()

	// 刷新本地文件列表
	fyne.Do(func() {
		fm.refreshLocalFiles()
	})
	return nil
}

// 添加下载文件的函数
func (fm *FileManagerApp) downloadFile() {
	if fm.selectedRemoteFile == nil {
		fm.statusLabel.SetText("请先选择一个远程文件")
		return
	}

	if fm.rpcNode == nil || !fm.rpcNode.handshaked {
		fm.statusLabel.SetText("请先连接到远程设备")
		return
	}

	file := fm.selectedRemoteFile
	filename := file.Name
	filesize := file.Size
	file_path := file.Path

	// 构造本地文件路径
	localPath := filepath.Join(fm.localCurrentPath, filename)

	fm.statusLabel.SetText(fmt.Sprintf("开始下载文件: %s", filename))
	fm.progressBar.Show()

	// 启动一个 goroutine 来执行下载，避免阻塞 UI
	go func() {
		err := fm.performDownload(file_path, localPath, filesize)
		fyne.CurrentApp().SendNotification(&fyne.Notification{
			Title:   "文件下载完成",
			Content: fmt.Sprintf("文件 %s 下载%s", filename, map[bool]string{true: "成功", false: "失败"}[err == nil]),
		})

		// 在主线程中更新 UI
		// fm.window.Canvas().(fyne.Canvas).Content().Refresh()
	}()
}

// 更新进度条的辅助函数
func (fm *FileManagerApp) updateProgress(progress float64, filename string) {
	fyne.Do(func() {
		fm.progressBar.SetValue(progress)
		percentage := int(progress * 100)
		fm.progressBar.TextFormatter = func() string {
			return fmt.Sprintf("%s - %d%%", filepath.Base(filename), percentage)
		}
		fm.progressBar.Refresh()
	})
}

// 隐藏进度条的辅助函数
func (fm *FileManagerApp) hideProgressBar() {
	fyne.Do(func() {
		fm.progressBar.Hide()
		fm.progressBar.Refresh()
	})
}

// 更新状态栏文本的辅助函数
func (fm *FileManagerApp) updateStatus(text string) {
	fyne.Do(func() {
		fm.statusLabel.SetText(text)
	})
}
func (fm *FileManagerApp) createMainContainer() *fyne.Container {
	// 本地文件管理器
	localContainer := fm.createLocalFileManager()

	// 远程文件管理器
	remoteContainer := fm.createRemoteFileManager()

	// 左右分割
	split := container.NewHSplit(localContainer, remoteContainer)
	split.SetOffset(0.5) // 均分

	return container.NewBorder(nil, nil, nil, nil, split)
}

func (fm *FileManagerApp) createLocalFileManager() *fyne.Container {
	// 创建面包屑导航
	fm.localBreadcrumb = widget.NewList(
		func() int {
			return len(fm.localPathStack)
		},
		func() fyne.CanvasObject {
			return widget.NewButton("", nil)
		},
		func(i widget.ListItemID, o fyne.CanvasObject) {
			btn := o.(*widget.Button)
			path := fm.localPathStack[i]
			switch path {
			case "\\":
				btn.SetText("根目录")
			case "/":
				btn.SetText("根目录")
			default:
				btn.SetText(filepath.Base(path))
			}
			btn.OnTapped = func() {
				fm.navigateToLocalPath(i)
			}
		},
	)

	// 创建文件网格
	fm.localFileGrid = widget.NewList(
		func() int {
			files := fm.getLocalFiles()
			return len(files)
		},
		func() fyne.CanvasObject {
			return widget.NewButton("", nil)
		},
		func(i widget.ListItemID, o fyne.CanvasObject) {
			btn := o.(*widget.Button)
			files := fm.getLocalFiles()
			if i < len(files) {
				file := files[i]
				displayName := file.Name
				if file.IsDir {
					displayName = "[]" + file.Name
				}
				btn.SetText(displayName)
				btn.OnTapped = func() {
					fm.onLocalFileClick(file)
				}
			}
		},
	)

	breadcrumbCard := widget.NewCard("本地目录导航", "", fm.localBreadcrumb)
	fileGridCard := widget.NewCard("本地文件", "", fm.localFileGrid)

	split := container.NewHSplit(breadcrumbCard, fileGridCard)
	return container.NewBorder(nil, nil, nil, nil, split)
}

func (fm *FileManagerApp) createRemoteFileManager() *fyne.Container {
	// 创建面包屑导航
	fm.remoteBreadcrumb = widget.NewList(
		func() int {
			return len(fm.remotePathStack)
		},
		func() fyne.CanvasObject {
			return widget.NewButton("", nil)
		},
		func(i widget.ListItemID, o fyne.CanvasObject) {
			btn := o.(*widget.Button)
			path := fm.remotePathStack[i]
			if path == "/" {
				btn.SetText("根目录")
			} else {
				btn.SetText(filepath.Base(path))
			}
			btn.OnTapped = func() {
				fm.navigateToRemotePath(i)
			}
		},
	)

	// 创建文件网格
	fm.remoteFileGrid = widget.NewList(
		func() int {
			files := fm.getRemoteFiles()
			return len(files)
		},
		func() fyne.CanvasObject {
			return widget.NewButton("", nil)
		},
		func(i widget.ListItemID, o fyne.CanvasObject) {
			btn := o.(*widget.Button)
			files := fm.getRemoteFiles()
			if i < len(files) {
				file := files[i]
				displayName := file.Name
				if file.IsDir {
					displayName = "[]" + file.Name
				}
				btn.SetText(displayName)
				btn.OnTapped = func() {
					fm.onRemoteFileClick(file)
				}
			}
		},
	)

	breadcrumbCard := widget.NewCard("远程目录导航", "", fm.remoteBreadcrumb)
	fileGridCard := widget.NewCard("远程文件", "", fm.remoteFileGrid)

	split := container.NewHSplit(breadcrumbCard, fileGridCard)
	return container.NewBorder(nil, nil, nil, nil, split)
}

func (fm *FileManagerApp) getLocalFiles() []FileItem {
	var files []FileItem

	if runtime.GOOS == "windows" && fm.localCurrentPath == "\\" {
		// Windows 根目录，显示盘符
		for c := 'A'; c <= 'Z'; c++ {
			drive := string(c) + ":"
			if _, err := os.Stat(drive + "\\"); err == nil {
				files = append(files, FileItem{
					Name:    drive,
					IsDir:   true,
					Size:    0,
					ModTime: time.Now(),
				})
			}
		}
	} else {
		// 读取当前目录的文件
		entries, err := os.ReadDir(fm.localCurrentPath)
		if err != nil {
			log.Printf("读取目录失败: %v", err)
			return files
		}

		for _, entry := range entries {
			info, err := entry.Info()
			if err != nil {
				continue
			}

			files = append(files, FileItem{
				Name:    entry.Name(),
				IsDir:   entry.IsDir(),
				Size:    info.Size(),
				ModTime: info.ModTime(),
			})
		}
	}

	// 排序：文件夹在前，按名称排序
	sort.Slice(files, func(i, j int) bool {
		if files[i].IsDir != files[j].IsDir {
			return files[i].IsDir
		}
		return files[i].Name < files[j].Name
	})

	return files
}

func (fm *FileManagerApp) getRemoteFiles() []FileItem {
	var files []FileItem
	// 如果路径未变化，直接返回缓存的文件列表
	if fm.last_remoteCurrentPath == fm.remoteCurrentPath {
		return fm.files
	}

	if fm.rpcNode == nil || fm.rpcNode.handshaked == false {
		return []FileItem{} // 如果未连接远程服务器，则返回空列表
	}
	isWindows, _ := fm.rpcNode.Call("OsIsWindows")
	if isWindows[0].(bool) && (fm.remoteCurrentPath == "\\" || fm.remoteCurrentPath == "/") {
		// Windows 根目录，显示盘符
		for c := 'A'; c <= 'Z'; c++ {
			drive := string(c) + ":" + "\\"
			_, err := fm.rpcNode.Call("Stat", drive)
			if err == nil {
				files = append(files, FileItem{
					Name:    drive,
					IsDir:   true,
					Size:    0,
					ModTime: time.Now(),
				})
			}
		}
	} else {
		// 读取当前目录的文件
		result, err := fm.rpcNode.Call("ReadDir", fm.remoteCurrentPath)
		if err != nil {
			log.Printf("读取远程目录失败: %v", err)
			return files // 返回空的 files 切片或其他错误处理
		}
		dirEntries := result[0].([]DirEntryData)
		for _, entry := range dirEntries {
			info := entry.Info // 修复点：直接访问 Info 字段
			// 构建完整路径
			fullPath := filepath.Join(fm.remoteCurrentPath, entry.Name)

			files = append(files, FileItem{
				Name:    entry.Name,
				IsDir:   entry.IsDir,
				Size:    info.Size,
				ModTime: time.Unix(info.ModTime, 0),
				Path:    fullPath, // 添加完整路径
			})
		}
	}
	// 排序：文件夹在前，按名称排序
	sort.Slice(files, func(i, j int) bool {
		if files[i].IsDir != files[j].IsDir {
			return files[i].IsDir
		}
		return files[i].Name < files[j].Name
	})

	fm.last_remoteCurrentPath = fm.remoteCurrentPath
	fm.files = files
	return files
	/*
		// 这里是远程文件的模拟数据，实际应该从远程服务器获取
		return []FileItem{
			{Name: "Documents", IsDir: true, Size: 0, ModTime: time.Now()},
			{Name: "Pictures", IsDir: true, Size: 0, ModTime: time.Now()},
			{Name: "readme.txt", IsDir: false, Size: 1024, ModTime: time.Now()},
			{Name: "config.json", IsDir: false, Size: 512, ModTime: time.Now()},
		}
	*/
}

func (fm *FileManagerApp) onLocalFileClick(file FileItem) {
	if file.IsDir {
		// 进入目录
		var newPath string
		if runtime.GOOS == "windows" && fm.localCurrentPath == "\\" {
			newPath = file.Name + "\\"
		} else {
			newPath = filepath.Join(fm.localCurrentPath, file.Name)
		}

		fm.localCurrentPath = newPath
		fm.localPathStack = append(fm.localPathStack, newPath)
		fm.refreshLocalFiles()
	} else {
		// 显示文件信息
		sizeStr := fm.formatFileSize(file.Size)
		timeStr := file.ModTime.Format("2006-01-02 15:04:05")
		info := fmt.Sprintf("文件: %s | 大小: %s | 修改时间: %s",
			file.Name, sizeStr, timeStr)
		fm.statusLabel.SetText(info)
	}
}

func (fm *FileManagerApp) onRemoteFileClick(file FileItem) {
	if file.IsDir {
		isWindows, _ := fm.rpcNode.Call("OsIsWindows")

		// 进入目录
		var newPath string
		if isWindows[0].(bool) && (fm.remoteCurrentPath == "\\" || fm.remoteCurrentPath == "/") {
			newPath = file.Name + "\\"
		} else {
			newPath = filepath.Join(fm.remoteCurrentPath, file.Name)
		}

		fm.remoteCurrentPath = newPath
		fm.remotePathStack = append(fm.remotePathStack, newPath)
		fm.refreshRemoteFiles()

		// 进入远程目录（这里只是模拟）
		/*
			newPath := filepath.Join(fm.remoteCurrentPath, file.Name)
			fm.remoteCurrentPath = newPath
			fm.remotePathStack = append(fm.remotePathStack, newPath)
			fm.refreshRemoteFiles()
		*/
	} else {
		fm.selectedRemoteFile = &file
		// 显示远程文件信息
		sizeStr := fm.formatFileSize(file.Size)
		timeStr := file.ModTime.Format("2006-01-02 15:04:05")
		info := fmt.Sprintf("远程文件: %s | 大小: %s | 修改时间: %s",
			file.Name, sizeStr, timeStr)
		fm.statusLabel.SetText(info)
	}
}

func (fm *FileManagerApp) navigateToLocalPath(index int) {
	if index < len(fm.localPathStack) {
		fm.localPathStack = fm.localPathStack[:index+1]
		fm.localCurrentPath = fm.localPathStack[index]
		fm.refreshLocalFiles()
	}
}

func (fm *FileManagerApp) navigateToRemotePath(index int) {
	if index < len(fm.remotePathStack) {
		fm.remotePathStack = fm.remotePathStack[:index+1]
		fm.remoteCurrentPath = fm.remotePathStack[index]
		fm.refreshRemoteFiles()
	}
}

func (fm *FileManagerApp) refreshLocalFiles() {
	fm.localBreadcrumb.Refresh()
	fm.localFileGrid.Refresh()
}

func (fm *FileManagerApp) refreshRemoteFiles() {
	if fm.rpcNode == nil || fm.rpcNode.handshaked == false {
		return // 如果未连接远程服务器，则不刷新远程文件列表
	}
	fm.remoteBreadcrumb.Refresh()
	fm.remoteFileGrid.Refresh()
}

func (fm *FileManagerApp) formatFileSize(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	div, exp := int64(unit), 0
	for n := size / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(size)/float64(div), "KMGTPE"[exp])
}

//go:embed assets/app_icon.png
var iconData []byte
var assets embed.FS

func fyne_main() {
	myApp := app.NewWithID("fyne-file-browser")
	myApp.Settings().SetTheme(NewCustomTheme(0.7)) // 设置自定义主题
	myWindow := myApp.NewWindow("串口文件交换")
	myWindow.Resize(fyne.NewSize(800, 400))

	// 创建资源
	icon := &fyne.StaticResource{
		StaticName:    "app_icon.png",
		StaticContent: iconData,
	}
	// 设置应用图标
	myApp.SetIcon(icon)
	fileManager := &FileManagerApp{
		window:      myWindow,
		statusLabel: widget.NewLabel("就绪"),
	}

	// 初始化路径
	fileManager.initializePaths()

	// 创建界面
	content := fileManager.createUI()
	myWindow.SetContent(content)

	// 初始化本地文件浏览器
	fileManager.refreshLocalFiles()

	myWindow.ShowAndRun()
}
