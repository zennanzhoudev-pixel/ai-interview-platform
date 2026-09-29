package recording

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestFSStoreAssemblesChunksInOrder(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFSStore(dir)
	if err != nil {
		t.Fatalf("创建存储失败: %v", err)
	}
	ctx := context.Background()
	key := "acme-cn/s_1/video.webm"

	chunks := [][]byte{[]byte("aaa"), []byte("bbb"), []byte("ccc")}
	// 故意乱序上传: 网络重试与并发上传都会让分片到达顺序不可控。
	for _, i := range []int{2, 0, 1} {
		if err := s.AppendChunk(ctx, key, i, chunks[i]); err != nil {
			t.Fatalf("写入分片 %d 失败: %v", i, err)
		}
	}
	if s.Exists(ctx, key) {
		t.Fatal("未合并前不应视为已完成")
	}

	size, err := s.Finalize(ctx, key, len(chunks))
	if err != nil {
		t.Fatalf("合并分片失败: %v", err)
	}
	if size != 9 {
		t.Fatalf("合并后大小应为 9, 实际 %d", size)
	}
	if !s.Exists(ctx, key) {
		t.Fatal("合并后应存在")
	}

	r, got, err := s.Open(ctx, key)
	if err != nil {
		t.Fatalf("打开录制件失败: %v", err)
	}
	defer r.Close()
	if got != 9 {
		t.Fatalf("打开时返回的大小应为 9, 实际 %d", got)
	}
	data, _ := io.ReadAll(r)
	if string(data) != "aaabbbccc" {
		t.Fatalf("分片未按序号拼接: %q", data)
	}
}

func TestFSStoreChunkRetransmissionIsIdempotent(t *testing.T) {
	s, _ := NewFSStore(t.TempDir())
	ctx := context.Background()
	key := "t/s/audio.webm"

	if err := s.AppendChunk(ctx, key, 0, []byte("first")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	// 同一个序号重传必须覆盖, 而不是追加 —— 追加会写出损坏的文件。
	if err := s.AppendChunk(ctx, key, 0, []byte("second")); err != nil {
		t.Fatalf("重传失败: %v", err)
	}
	if _, err := s.Finalize(ctx, key, 1); err != nil {
		t.Fatalf("合并失败: %v", err)
	}
	r, _, _ := s.Open(ctx, key)
	defer r.Close()
	data, _ := io.ReadAll(r)
	if string(data) != "second" {
		t.Fatalf("重传应覆盖旧分片, 实际 %q", data)
	}
}

func TestFSStoreReportsIncompleteChunks(t *testing.T) {
	s, _ := NewFSStore(t.TempDir())
	ctx := context.Background()
	key := "t/s/video.webm"

	if err := s.AppendChunk(ctx, key, 0, []byte("only")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if _, err := s.Finalize(ctx, key, 3); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("缺分片应返回 ErrIncomplete, 实际 %v", err)
	}
	// 失败后不应留下半成品文件, 否则断点续传会把它当成完整件。
	if s.Exists(ctx, key) {
		t.Fatal("合并失败不应留下最终文件")
	}
}

func TestFSStoreRejectsPathTraversal(t *testing.T) {
	s, _ := NewFSStore(t.TempDir())
	ctx := context.Background()
	bad := []string{
		"../../etc/passwd",
		"/absolute/path",
		"tenant/../../../secret",
		"",
		"with space",
	}
	for _, key := range bad {
		if err := s.AppendChunk(ctx, key, 0, []byte("x")); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("键 %q 应被拒绝, 实际 %v", key, err)
		}
		if _, _, err := s.Open(ctx, key); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("打开键 %q 应被拒绝, 实际 %v", key, err)
		}
	}
}

func TestFSStoreRemoveCleansPartsAndFile(t *testing.T) {
	root := t.TempDir()
	s, _ := NewFSStore(root)
	ctx := context.Background()
	key := "t/s/video.webm"

	_ = s.AppendChunk(ctx, key, 0, []byte("data"))
	if _, err := s.Finalize(ctx, key, 1); err != nil {
		t.Fatalf("合并失败: %v", err)
	}
	if err := s.Remove(ctx, key); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if s.Exists(ctx, key) {
		t.Fatal("删除后不应再存在")
	}
	// 分片目录也必须清掉, 否则磁盘会被"已删除"的录制件悄悄占满。
	if _, err := os.Stat(filepath.Join(root, ".parts", filepath.FromSlash(key))); !os.IsNotExist(err) {
		t.Fatalf("分片目录未清理: %v", err)
	}
}

func TestFSStoreOpenMissingReturnsNotExist(t *testing.T) {
	s, _ := NewFSStore(t.TempDir())
	if _, _, err := s.Open(context.Background(), "t/s/missing.webm"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("打开不存在的录制件应返回 os.ErrNotExist, 实际 %v", err)
	}
}

func TestChunkIndexesSupportResume(t *testing.T) {
	s, _ := NewFSStore(t.TempDir())
	ctx := context.Background()
	_ = s.AppendChunk(ctx, "t/s/v.webm", 0, []byte{1})
	_ = s.AppendChunk(ctx, "t/s/v.webm", 2, []byte{2})
	idx, err := s.ChunkIndexes("t/s/v.webm")
	if err != nil {
		t.Fatalf("列举分片失败: %v", err)
	}
	if len(idx) != 2 || idx[0] != 0 || idx[1] != 2 {
		t.Fatalf("分片序号不正确: %v", idx)
	}
}

func TestValidateKeyAcceptsNormalKeys(t *testing.T) {
	ok := []string{"t/s/video.webm", "acme-cn/s_1/audio.webm", "a/b/c/d.webm"}
	for _, k := range ok {
		if err := ValidateKey(k); err != nil {
			t.Errorf("合法键 %q 被拒绝: %v", k, err)
		}
	}
	if err := ValidateKey(string(bytes.Repeat([]byte("a"), 300))); err == nil {
		t.Error("超长键应被拒绝")
	}
}
