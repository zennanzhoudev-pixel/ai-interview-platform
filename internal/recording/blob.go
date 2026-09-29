// Package recording 保存面试的音视频录制件。
//
// 三个设计决定, 每个都是被真实事故逼出来的:
//
//  1. **分片上传**: 面试录像在浏览器侧由 MediaRecorder 边录边产生,
//     一次 45 分钟的会议是几百 MB。等录完再上传意味着"候选人关页面就全丢",
//     以及"上传期间网络抖动要重传全部"。按时间片上传, 断线只丢一片。
//
//  2. **分片可重传**: 每个分片单独落盘成文件, 重传同一序号直接覆盖。
//     网络重试是常态, 追加式写入在重试下必然写出损坏的容器文件。
//
//  3. **二进制不进数据库**: 数据库只存元数据(见 store.Recording),
//     内容落对象存储或文件系统。把视频塞进 MySQL 会同时拖垮
//     备份、主从复制和缓冲池 —— 而且是在秋招那几天。
package recording

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// ErrInvalidKey 表示存储键不合法。
var ErrInvalidKey = errors.New("recording: 非法的存储键")

// ErrIncomplete 表示分片不齐, 无法合并。
var ErrIncomplete = errors.New("recording: 分片不完整")

// keyRE 限定存储键的字符集。
//
// 存储键来自服务端拼接(租户/会话/类型), 但一旦拼进文件路径, 校验就必须
// 存在: 未来某次改动把 session_id 直接透传时, "../../etc/passwd" 就会
// 变成写文件路径。校验放在入口, 而不是指望每个调用方都记得。
var keyRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,220}$`)

// ValidateKey 校验存储键。
func ValidateKey(key string) error {
	if !keyRE.MatchString(key) {
		return fmt.Errorf("%w: %q", ErrInvalidKey, key)
	}
	if strings.Contains(key, "..") || strings.HasPrefix(key, "/") {
		return fmt.Errorf("%w: %q", ErrInvalidKey, key)
	}
	return nil
}

// BlobStore 是录制件内容的存储抽象。
type BlobStore interface {
	// AppendChunk 写入一个分片。同一序号重复写入必须覆盖且幂等。
	AppendChunk(ctx context.Context, key string, index int, data []byte) error
	// Finalize 按序号合并分片, 返回最终字节数。缺少序号时必须返回 ErrIncomplete。
	Finalize(ctx context.Context, key string, chunks int) (int64, error)
	// Open 打开已合并的录制件。
	Open(ctx context.Context, key string) (io.ReadSeekCloser, int64, error)
	// Remove 删除录制件与它的全部分片。
	Remove(ctx context.Context, key string) error
	// Exists 判断录制件是否已合并完成。
	Exists(ctx context.Context, key string) bool
}

// FSStore 是基于本地文件系统的实现。
//
// 它适合单机部署与本地联调; 多副本部署时应换成对象存储实现 ——
// 换的时候只需要实现这个接口, 上层(分片协议、审计、权限)一行都不用改。
type FSStore struct {
	root string
	mu   sync.Mutex
}

// NewFSStore 创建文件系统存储。目录不存在时自动创建。
func NewFSStore(root string) (*FSStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("recording: 存储根目录不能为空")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o750); err != nil {
		return nil, fmt.Errorf("recording: 创建存储目录失败: %w", err)
	}
	return &FSStore{root: abs}, nil
}

// Root 返回存储根目录(便于运维确认落盘位置)。
func (f *FSStore) Root() string { return f.root }

func (f *FSStore) path(key string) (string, error) {
	if err := ValidateKey(key); err != nil {
		return "", err
	}
	full := filepath.Join(f.root, filepath.FromSlash(key))
	// 二次确认: 即使用户键通过了正则, 也要保证落点仍在根目录内。
	if !strings.HasPrefix(full, f.root+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: %q", ErrInvalidKey, key)
	}
	return full, nil
}

func (f *FSStore) partDir(key string) (string, error) {
	if err := ValidateKey(key); err != nil {
		return "", err
	}
	return filepath.Join(f.root, ".parts", filepath.FromSlash(key)), nil
}

// AppendChunk 写入一个分片。
func (f *FSStore) AppendChunk(_ context.Context, key string, index int, data []byte) error {
	if index < 0 || index > 1_000_000 {
		return fmt.Errorf("recording: 分片序号越界: %d", index)
	}
	dir, err := f.partDir(key)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	part := filepath.Join(dir, fmt.Sprintf("%06d.part", index))
	// 先写临时文件再改名: 中途失败不会留下一个"看起来存在但内容不全"
	// 的分片, 而合并阶段无法区分这两者。
	tmp := part + ".tmp"
	if err := os.WriteFile(tmp, data, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, part)
}

// Finalize 合并分片。
func (f *FSStore) Finalize(_ context.Context, key string, chunks int) (int64, error) {
	full, err := f.path(key)
	if err != nil {
		return 0, err
	}
	dir, err := f.partDir(key)
	if err != nil {
		return 0, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if chunks <= 0 {
		// 未声明分片数时按目录内实际分片数合并, 仍然要求序号从 0 连续。
		entries, err := os.ReadDir(dir)
		if err != nil {
			return 0, fmt.Errorf("%w: 没有可合并的分片", ErrIncomplete)
		}
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".part") {
				chunks++
			}
		}
	}
	if chunks == 0 {
		return 0, fmt.Errorf("%w: 收到 0 个分片", ErrIncomplete)
	}

	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		return 0, err
	}
	tmp := full + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	var total int64
	for i := 0; i < chunks; i++ {
		part := filepath.Join(dir, fmt.Sprintf("%06d.part", i))
		in, err := os.Open(part)
		if err != nil {
			_ = out.Close()
			_ = os.Remove(tmp)
			return 0, fmt.Errorf("%w: 缺少第 %d 个分片", ErrIncomplete, i)
		}
		n, err := io.Copy(out, in)
		_ = in.Close()
		if err != nil {
			_ = out.Close()
			_ = os.Remove(tmp)
			return 0, err
		}
		total += n
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	if err := os.Rename(tmp, full); err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	_ = os.RemoveAll(dir)
	return total, nil
}

// Open 打开已合并的录制件, 供 Range 播放。
func (f *FSStore) Open(_ context.Context, key string) (io.ReadSeekCloser, int64, error) {
	full, err := f.path(key)
	if err != nil {
		return nil, 0, err
	}
	file, err := os.Open(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, os.ErrNotExist
		}
		return nil, 0, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, 0, err
	}
	return file, info.Size(), nil
}

// Remove 删除录制件与全部分片。
func (f *FSStore) Remove(_ context.Context, key string) error {
	full, err := f.path(key)
	if err != nil {
		return err
	}
	dir, err := f.partDir(key)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	errFinal := os.Remove(full)
	errParts := os.RemoveAll(dir)
	if errFinal != nil && !os.IsNotExist(errFinal) {
		return errFinal
	}
	if errParts != nil && !os.IsNotExist(errParts) {
		return errParts
	}
	return nil
}

// Exists 判断录制件是否已合并完成。
func (f *FSStore) Exists(_ context.Context, key string) bool {
	full, err := f.path(key)
	if err != nil {
		return false
	}
	info, err := os.Stat(full)
	return err == nil && !info.IsDir()
}

// ChunkIndexes 返回某个键已上传的分片序号(便于断点续传)。
func (f *FSStore) ChunkIndexes(key string) ([]int, error) {
	dir, err := f.partDir(key)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil
	}
	out := make([]int, 0, len(entries))
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), ".part")
		if name == e.Name() {
			continue
		}
		var idx int
		if _, err := fmt.Sscanf(name, "%06d", &idx); err == nil {
			out = append(out, idx)
		}
	}
	sort.Ints(out)
	return out, nil
}
