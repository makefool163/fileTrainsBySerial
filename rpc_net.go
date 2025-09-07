package main

import (
	"encoding/gob"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"reflect"
	"sync"
	"time"
)

// RPCServer 代表 RPC 服務端
type RPCServer struct {
	addr      string
	functions map[string]reflect.Value
	mu        sync.RWMutex
	listener  net.Listener
	started   chan struct{} // 添加啟動信號
}

// NewRPCServer 創建一個新的 RPC 服務端
func NewRPCServer(addr string) *RPCServer {
	// 註冊 gob 類型
	gob.Register([]DirEntryData{})
	gob.Register(DirEntryData{})
	gob.Register(FileInfoData{})

	return &RPCServer{
		addr:      addr,
		functions: make(map[string]reflect.Value),
		started:   make(chan struct{}),
	}
}

// Start 啟動服務端
func (s *RPCServer) Start() error {
	listener, err := net.Listen("tcp", s.addr)
	if err != nil {
		close(s.started) // 確保等待的 goroutine 不會永遠阻塞
		return err
	}

	s.listener = listener
	close(s.started) // 通知服務端已啟動
	fmt.Printf("RPC 服務端監聽於 %s\n", s.addr)

	for {
		conn, err := listener.Accept()
		if err != nil {
			// 檢查是否是因為關閉 listener 導致的錯誤
			if netErr, ok := err.(net.Error); ok && netErr.Temporary() {
				continue
			}
			return err
		}
		go s.handleConnection(conn)
	}
}

// WaitForStart 等待服務端啟動完成
func (s *RPCServer) WaitForStart() {
	<-s.started
}

func (s *RPCServer) Stop() error {
	if s.listener != nil {
		return s.listener.Close()
	}
	return nil
}

// Register 註冊一個函數到 RPC 服務端
func (s *RPCServer) Register(name string, f interface{}) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	fVal := reflect.ValueOf(f)
	if fVal.Kind() != reflect.Func {
		return errors.New("註冊的必須是函數")
	}
	if _, exists := s.functions[name]; exists {
		return errors.New("函數已存在")
	}
	s.functions[name] = fVal
	return nil
}

// handleConnection 處理單個客戶端連接，支持並發
func (s *RPCServer) handleConnection(conn net.Conn) {
	defer conn.Close()

	decoder := gob.NewDecoder(conn)
	encoder := gob.NewEncoder(conn)

	for {
		// 解碼請求
		var req struct {
			FuncName string
			Args     []any
		}

		if err := decoder.Decode(&req); err != nil {
			// 如果解碼失敗，返回錯誤信息給客戶端
			var resp struct {
				Result []any
				Error  string
			}
			resp.Error = fmt.Sprintf("解碼請求失敗: %v", err)

			// 嘗試發送錯誤響應
			_ = encoder.Encode(resp)
			return
		}

		s.mu.RLock()
		fVal, ok := s.functions[req.FuncName]
		s.mu.RUnlock()

		var resp struct {
			Result []any
			Error  string
		}

		if !ok {
			resp.Error = "函數未找到"
		} else {
			// 準備參數
			fType := fVal.Type()
			inArgs := make([]reflect.Value, len(req.Args))
			if fType.NumIn() != len(req.Args) {
				resp.Error = "參數數量不匹配"
			} else {
				for i := range req.Args {
					// 檢查參數是否為 nil
					if req.Args[i] == nil {
						// 對於 nil 參數，創建對應類型的零值
						inArgs[i] = reflect.Zero(fType.In(i))
						continue
					}

					inArgs[i] = reflect.ValueOf(req.Args[i])
					if !inArgs[i].Type().ConvertibleTo(fType.In(i)) {
						resp.Error = fmt.Sprintf("參數類型不匹配 at %d", i)
						break
					}
					inArgs[i] = inArgs[i].Convert(fType.In(i))
				}

				if resp.Error == "" {
					// 調用函數
					out := fVal.Call(inArgs)
					resp.Result = make([]any, len(out))
					for i := range out {
						// 處理特殊返回值類型
						switch v := out[i].Interface().(type) {
						case []fs.DirEntry:
							data, err := ToDirEntryData(v)
							if err != nil {
								resp.Error = fmt.Sprintf("序列化 DirEntry 失敗: %v", err)
								break
							} else {
								resp.Result[i] = data
							}
						case fs.FileInfo:
							resp.Result[i] = ToFileInfoData(v)
						default:
							resp.Result[i] = v
						}
					}
				}
			}
		}

		// 發送回應
		if err := encoder.Encode(resp); err != nil {
			fmt.Printf("發送響應失敗: %v\n", err)
			// 即使發送失敗，也繼續處理下一個請求
			continue
		}
	}
}

// RPCClient 代表 RPC 客戶端
type RPCClient struct {
	conn    net.Conn
	encoder *gob.Encoder
	decoder *gob.Decoder
	mu      sync.Mutex
}

// NewRPCClient 創建一個新的 RPC 客戶端，支持重試
func NewRPCClient(addr string) (*RPCClient, error) {
	var conn net.Conn
	var err error

	// 添加重試機制
	for i := 0; i < 5; i++ {
		conn, err = net.DialTimeout("tcp", addr, 5*time.Second)
		if err == nil {
			break
		}
		time.Sleep(time.Millisecond * 100 * time.Duration(i+1)) // 漸進式延遲
	}

	if err != nil {
		return nil, fmt.Errorf("無法連接到服務端 %s: %w", addr, err)
	}

	// 在客戶端註冊 gob 類型
	gob.Register([]DirEntryData{})
	gob.Register(DirEntryData{})
	gob.Register(FileInfoData{})

	return &RPCClient{
		conn:    conn,
		encoder: gob.NewEncoder(conn),
		decoder: gob.NewDecoder(conn),
	}, nil
}

// Call 調用遠端函數
func (c *RPCClient) Call(funcName string, args ...any) ([]any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// 發送請求
	req := struct {
		FuncName string
		Args     []any
	}{
		FuncName: funcName,
		Args:     args,
	}

	// 設置寫入超時
	c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := c.encoder.Encode(req); err != nil {
		return nil, fmt.Errorf("發送請求失敗: %w", err)
	}

	// 接收回應
	// 設置讀取超時
	c.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var resp struct {
		Result []any
		Error  string
	}
	if err := c.decoder.Decode(&resp); err != nil {
		return nil, fmt.Errorf("接收響應失敗: %w", err)
	}
	if resp.Error != "" {
		return nil, errors.New(resp.Error)
	}
	return resp.Result, nil
}

// Close 關閉客戶端連接
func (c *RPCClient) Close() error {
	return c.conn.Close()
}

// 示例使用
func rpc_base_main() {
	// 註冊 os.ReadDir 和 os.Stat
	server := NewRPCServer("localhost:8080")
	if err := server.Register("ReadDir", os.ReadDir); err != nil {
		fmt.Printf("註冊 ReadDir 失敗: %v\n", err)
		return
	}
	if err := server.Register("Stat", os.Stat); err != nil {
		fmt.Printf("註冊 Stat 失敗: %v\n", err)
		return
	}

	go func() {
		if err := server.Start(); err != nil {
			fmt.Printf("服務端啟動失敗: %v\n", err)
		}
	}()

	// 等待服務端啟動
	server.WaitForStart()
	time.Sleep(200 * time.Millisecond) // 增加延遲確保服務端完全準備好

	// 客戶端
	client, err := NewRPCClient("localhost:8080")
	if err != nil {
		fmt.Printf("客戶端連接失敗: %v\n", err)
		return
	}
	defer func() {
		if err := client.Close(); err != nil {
			fmt.Printf("關閉客戶端失敗: %v\n", err)
		}
	}()

	// 調用 ReadDir
	fmt.Println("調用 ReadDir...")
	results, err := client.Call("ReadDir", ".")
	if err != nil {
		fmt.Printf("調用 ReadDir 失敗: %v\n", err)
		return
	}
	dirEntries := results[0].([]DirEntryData)
	for _, entry := range dirEntries {
		fmt.Printf("DirEntry: %s, IsDir: %v\n", entry.Name, entry.IsDir)
	}

	// 調用 Stat
	fmt.Println("調用 Stat...")
	results, err = client.Call("Stat", "rpc_base.go")
	if err != nil {
		fmt.Printf("調用 Stat 失敗: %v\n", err)
		return
	}
	fileInfo := results[0].(FileInfoData)
	fmt.Printf("FileInfo: %s, Size: %d, IsDir: %v\n", fileInfo.Name, fileInfo.Size, fileInfo.IsDir)
}
