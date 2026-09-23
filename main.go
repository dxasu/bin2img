// bin2img
//
// 用法: bin2img [选项] <二进制文件>
//
// 功能：
//   - 参数是【已编译好的二进制】，不再负责编译 Go 源码
//   - 构建期把 ./oci 目录整体嵌入程序（go:embed），
//     manifest.json / repositories 等模板取自嵌入内容
//   - 运行期生成 docker save 兼容的【单个 tar 镜像文件】：
//       <layer>/layer.tar   单文件 layer（未压缩 tar，内含 /<name>，0755）
//       <layer>/VERSION     "1.0"
//       <layer>/json        旧版 layer 元数据
//       <config>.json       镜像 config（文件名 = 其 sha256）
//       manifest.json       docker load 的入口
//       repositories        仓库标签映射
//   - 输出可直接 docker load / podman load
//
// 示例：
//   go build -o bin2img .
//   ./bin2img server                  # 生成 ./server.tar
//   ./bin2img -o app.tar app          # 指定输出文件
//   ./bin2img -name app -tag my/app:v2 -arch arm64 app

package main

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ---------- 嵌入的模板（构建期打包自 ./oci 目录） ----------

//go:embed oci
var ociTemplate embed.FS

// ---------- 结构定义 ----------

type runtimeConfig struct {
	Env        []string `json:"Env,omitempty"`
	Entrypoint []string `json:"Entrypoint"`
}

type historyEntry struct {
	Created    string `json:"created"`
	CreatedBy  string `json:"created_by,omitempty"`
	EmptyLayer bool   `json:"empty_layer,omitempty"`
}

// docker 镜像 config（image spec v1）
type imageConfig struct {
	Architecture string         `json:"architecture"`
	OS           string         `json:"os"`
	Created      string         `json:"created,omitempty"`
	Config       runtimeConfig  `json:"config"`
	History      []historyEntry `json:"history,omitempty"`
	RootFS       struct {
		Type    string   `json:"type"`
		DiffIDs []string `json:"diff_ids"`
	} `json:"rootfs"`
}

// docker save 兼容的 manifest.json
type dockerArchiveManifest struct {
	Config   string   `json:"Config"`
	RepoTags []string `json:"RepoTags"`
	Layers   []string `json:"Layers"`
}

// 旧版 <layer>/json 元数据（docker load 向后兼容用）
type legacyLayerJSON struct {
	ID              string         `json:"id"`
	Created         string         `json:"created,omitempty"`
	OS              string         `json:"os,omitempty"`
	Architecture    string         `json:"architecture,omitempty"`
	Config          *runtimeConfig `json:"config,omitempty"`
	ContainerConfig *runtimeConfig `json:"container_config,omitempty"`
}

// ---------- 工具函数 ----------

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "❌ "+format+"\n", args...)
	os.Exit(1)
}

func mustReadTemplate(name string) []byte {
	b, err := ociTemplate.ReadFile(name)
	if err != nil {
		fatalf("读取嵌入模板 %s 失败: %v", name, err)
	}
	return b
}

func mustJSONUnmarshal(data []byte, v any) {
	if err := json.Unmarshal(data, v); err != nil {
		fatalf("解析 JSON 失败: %v\n%s", err, data)
	}
}

func sha(b []byte) string {
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:])
}

func hexOf(digest string) string {
	return strings.TrimPrefix(digest, "sha256:")
}

func splitTag(s string) (repo, tag string) {
	if i := strings.LastIndex(s, ":"); i > 0 && !strings.Contains(s[i:], "/") {
		return s[:i], s[i+1:]
	}
	return s, "latest"
}

// ---------- 构建各组件 ----------

// buildLayer 把二进制打包成单文件 tar（entry 名为 name，0755），不压缩
func buildLayer(bin []byte, name string) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	// 若 name 含路径，补上父目录条目
	if dir := path.Dir(name); dir != "." {
		if err := tw.WriteHeader(&tar.Header{
			Name:     dir + "/",
			Mode:     0o755,
			Typeflag: tar.TypeDir,
		}); err != nil {
			return nil, err
		}
	}

	if err := tw.WriteHeader(&tar.Header{
		Name:     name,
		Mode:     0o755,
		Size:     int64(len(bin)),
		Typeflag: tar.TypeReg,
	}); err != nil {
		return nil, err
	}
	if _, err := tw.Write(bin); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func buildImageConfig(arch, osName, name, createdBy string, diffID string) ([]byte, error) {
	created := time.Now().UTC().Format(time.RFC3339Nano)
	var cfg imageConfig
	cfg.Architecture = arch
	cfg.OS = osName
	cfg.Created = created
	cfg.Config.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	cfg.Config.Entrypoint = []string{"/" + strings.TrimPrefix(name, "/")}
	cfg.History = []historyEntry{
		{Created: created, CreatedBy: createdBy},
		{Created: created, CreatedBy: fmt.Sprintf("bin2img: ENTRYPOINT [\"/%s\"]", name), EmptyLayer: true},
	}
	cfg.RootFS.Type = "layers"
	cfg.RootFS.DiffIDs = []string{diffID}
	return json.MarshalIndent(cfg, "", "  ")
}

func buildLegacyLayerJSON(id, arch, osName, created, name string) ([]byte, error) {
	j := legacyLayerJSON{
		ID:           id,
		Created:      created,
		OS:           osName,
		Architecture: arch,
		Config: &runtimeConfig{
			Env:        []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
			Entrypoint: []string{"/" + strings.TrimPrefix(name, "/")},
		},
		ContainerConfig: &runtimeConfig{},
	}
	return json.MarshalIndent(j, "", "  ")
}

// ---------- 输出 ----------

// writeDockerTar 把所有镜像文件打包成 docker save 兼容的单一 tar
func writeDockerTar(tarPath string, layerHex string, files map[string][]byte) error {
	f, err := os.Create(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()

	tw := tar.NewWriter(f)
	defer tw.Close()

	// layer 目录条目
	if err := tw.WriteHeader(&tar.Header{
		Name:     layerHex + "/",
		Mode:     0o755,
		Typeflag: tar.TypeDir,
	}); err != nil {
		return err
	}

	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		if err := tw.WriteHeader(&tar.Header{
			Name:     k,
			Mode:     0o644,
			Size:     int64(len(files[k])),
			Typeflag: tar.TypeReg,
		}); err != nil {
			return err
		}
		if _, err := tw.Write(files[k]); err != nil {
			return err
		}
	}
	return nil
}

// ---------- 主流程 ----------

func main() {
	var (
		out    = flag.String("o", "", "输出 tar 文件路径（默认 <name>.tar）")
		name   = flag.String("name", "", "二进制在镜像内的文件名（默认取参数的文件名）")
		tag    = flag.String("tag", "", "镜像标签（默认 <name>:scratch）")
		arch   = flag.String("arch", "amd64", "目标架构")
		osName = flag.String("os", "linux", "目标操作系统")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "用法: bin2img [选项] <二进制文件>\n\n选项:\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(1)
	}

	binPath := flag.Arg(0)
	bin, err := os.ReadFile(binPath)
	if err != nil {
		fatalf("读取二进制 %s 失败: %v", binPath, err)
	}
	if len(bin) == 0 {
		fatalf("%s 是空文件", binPath)
	}

	if *name == "" {
		*name = filepath.Base(binPath)
	}
	if *tag == "" {
		*tag = *name + ":scratch"
	}
	if *out == "" {
		*out = *name + ".tar"
	}
	repo, tagName := splitTag(*tag)

	// 1. 构造 layer：单文件未压缩 tar（docker save 格式要求 layer.tar 不压缩）
	fmt.Printf("📦 打包 %s (%d bytes) 为 layer ...\n", binPath, len(bin))
	tarBytes, err := buildLayer(bin, *name)
	if err != nil {
		fatalf("构造 layer tar 失败: %v", err)
	}
	diffID := sha(tarBytes) // diff_id = 未压缩 tar 流的 sha256
	layerHex := hexOf(diffID)

	// 2. 生成镜像 config（文件名 = 其 sha256 hex + .json）
	created := time.Now().UTC().Format(time.RFC3339Nano)
	configBytes, err := buildImageConfig(*arch, *osName, *name,
		fmt.Sprintf("genimage: COPY %s /%s", filepath.Base(binPath), *name), diffID)
	if err != nil {
		fatalf("序列化 config 失败: %v", err)
	}
	configHex := hexOf(sha(configBytes))

	// 3. 旧版 layer 元数据（docker load 向后兼容）
	legacyBytes, err := buildLegacyLayerJSON(layerHex, *arch, *osName, created, *name)
	if err != nil {
		fatalf("序列化 legacy json 失败: %v", err)
	}

	// 4. 基于嵌入模板修改 manifest.json
	var dj []dockerArchiveManifest
	mustJSONUnmarshal(mustReadTemplate("oci/manifest.json"), &dj)
	if len(dj) == 0 {
		dj = []dockerArchiveManifest{{}}
	}
	dj[0].Config = configHex + ".json"
	dj[0].RepoTags = []string{*tag}
	dj[0].Layers = []string{layerHex + "/layer.tar"}
	manifestBytes, err := json.MarshalIndent(dj, "", "  ")
	if err != nil {
		fatalf("序列化 manifest.json 失败: %v", err)
	}

	// 5. 基于嵌入模板修改 repositories
	repos := map[string]map[string]string{
		repo: {tagName: layerHex},
	}
	repoBytes, err := json.MarshalIndent(repos, "", "  ")
	if err != nil {
		fatalf("序列化 repositories 失败: %v", err)
	}

	// 6. 汇总所有文件，写出单一 tar
	files := map[string][]byte{
		configHex + ".json":          configBytes,
		layerHex + "/VERSION":        []byte("1.0\n"),
		layerHex + "/json":           legacyBytes,
		layerHex + "/layer.tar":      tarBytes,
		"manifest.json":              manifestBytes,
		"repositories":               repoBytes,
	}

	if err := writeDockerTar(*out, layerHex, files); err != nil {
		fatalf("写 %s 失败: %v", *out, err)
	}

	fmt.Println("✅ docker 镜像已生成（单文件 tar）")
	fmt.Printf("   输出      %s\n", *out)
	fmt.Printf("   标签      %s\n", *tag)
	fmt.Printf("   layer     %s (%d bytes, tar 内文件 /%s)\n", diffID, len(tarBytes), *name)
	fmt.Printf("   config    sha256:%s\n", configHex)
	fmt.Println()
	fmt.Printf("导入运行:\n   docker load -i %s\n   docker run --rm %s\n", *out, *tag)
}
