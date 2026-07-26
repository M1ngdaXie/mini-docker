package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// ——— 1. 数据结构：把 curl 看到的 JSON 翻译成 Go struct ———
//
// curl /manifests/latest 返回的 JSON：
//
//	{ "manifests": [ { "digest": "sha256:xxx", "platform": {"architecture":"amd64","os":"linux"} }, ... ] }
//
// 这个叫 manifest list（多架构索引）。
type ManifestList struct {
	SchemaVersion int             `json:"schemaVersion"`
	Manifests     []ManifestEntry `json:"manifests"`
}

type ManifestEntry struct {
	Digest   string   `json:"digest"`   // 指向真正 manifest 的 sha256
	Platform Platform `json:"platform"` // 比如 amd64/linux
}

type Platform struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
}

// curl /manifests/sha256:79ff19... 返回的 JSON：
//
//	{ "config": {"digest":"sha256:xxx"}, "layers": [{"digest":"sha256:yyy"}] }
//
// 这个是单架构的 manifest——里面有 config 和 layers。
type ImageManifest struct {
	SchemaVersion int       `json:"schemaVersion"`
	Config        BlobRef   `json:"config"` // 指向 config blob
	Layers        []BlobRef `json:"layers"` // 文件系统层，索引 0 是最底层
}

// BlobRef 是个"指针"——用 sha256 digest 指向 registry 上的一个文件。
// config blob 和每一层 layer blob 都用这个表示。
type BlobRef struct {
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

// curl /blobs/sha256:d529dd0c...（config blob）返回的 JSON：
//
//	{ "config": { "Env": ["PATH=..."], "Cmd": ["/bin/sh"] } }
//
// 只有 Env 和 Cmd 是我们需要的。
// ImageConfig 是 config blob 的 Go 表示。
// Docker 里实际执行的命令 = Entrypoint（如果有）+ Cmd
// 比如 redis: Entrypoint=["docker-entrypoint.sh"], Cmd=["redis-server"]
//
//	→ 实际执行: docker-entrypoint.sh redis-server
type ImageConfig struct {
	Config struct {
		Env        []string `json:"Env"`
		Cmd        []string `json:"Cmd"`
		Entrypoint []string `json:"Entrypoint"`
	} `json:"config"`
}

// PullResult 是 pullImage() 的返回值——run() 只需要这三样
type PullResult struct {
	LowerDirs  string   // 冒号分隔：/tmp/layers/.../sha1:/tmp/layers/.../sha2
	Env        []string // PATH, PYTHON_VERSION... 直接拼进 cmd.Env
	Cmd        []string // 镜像默认命令，用户没指定时用
	Entrypoint []string // 镜像入口脚本，如果有就排在 Cmd 前面
}

// ——— 2. 注册表 HTTP 客户端 ———

// getToken 向 Docker Hub 要一个临时 token。
// 免费公开镜像不需要登录——传 repo 名就行，返回一个 Bearer token。
// 所有后续请求都要带 Authorization: Bearer <token>
func getToken(repo string) (string, error) {
	url := fmt.Sprintf(
		"https://auth.docker.io/token?service=registry.docker.io&scope=repository:%s:pull",
		repo,
	)
	resp, err := http.Get(url)
	if err != nil {
		return "", fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()

	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("token decode: %w", err)
	}
	return out.Token, nil
}

// doRequest 封装了"带 token 和 Accept 头发一个 GET，把 JSON 解析到 target"。
// manifest list、manifest、config blob 共用这个函数——它们都是 JSON。
// layer blob 不走这个（layer 是 tar.gz 二进制）。
func doRequest(url, token, accept string, target interface{}) error {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: status %d", url, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(target)
}

// ——— 3. pullImage ——
// 输入 "alpine" + "latest" → 输出 PullResult{LowerDirs, Env, Cmd}
//
// 流程：
//  1. 解析镜像名（"alpine" → "library/alpine"）
//  2. 拿 token
//  3. GET /manifests/latest → ManifestList → 挑 amd64/linux
//  4. GET /manifests/sha256:xxx → ImageManifest → config.digest + layers[].digest
//  5. GET /blobs/config.digest → ImageConfig → Env + Cmd
//  6. 遍历 layers，每个 GET /blobs/digest → tar.gz → 解压到 /tmp/layers/
//  7. 返回
func pullImage(image, tag string) (*PullResult, error) {
	if tag == "" {
		tag = "latest"
	}
	repo := image
	if !strings.Contains(image, "/") {
		repo = "library/" + image // "alpine" → "library/alpine"
	}

	token, err := getToken(repo)
	if err != nil {
		return nil, err
	}

	baseURL := fmt.Sprintf("https://registry-1.docker.io/v2/%s", repo)

	// 1. manifest list → 挑 amd64
	var ml ManifestList
	if err := doRequest(
		fmt.Sprintf("%s/manifests/%s", baseURL, tag),
		token,
		"application/vnd.docker.distribution.manifest.v2+json",
		&ml,
	); err != nil {
		return nil, fmt.Errorf("manifest list: %w", err)
	}

	var targetDigest string
	for _, e := range ml.Manifests {
		if e.Platform.Architecture == "amd64" && e.Platform.OS == "linux" {
			targetDigest = e.Digest
			break
		}
	}
	if targetDigest == "" {
		return nil, fmt.Errorf("no amd64/linux manifest for %s:%s", image, tag)
	}

	// 2. 真正的 manifest → config.digest + layers
	var im ImageManifest
	if err := doRequest(
		fmt.Sprintf("%s/manifests/%s", baseURL, targetDigest),
		token,
		"application/vnd.oci.image.manifest.v1+json",
		&im,
	); err != nil {
		return nil, fmt.Errorf("image manifest: %w", err)
	}

	// 3. config blob → Env + Cmd
	var cfg ImageConfig
	if err := doRequest(
		fmt.Sprintf("%s/blobs/%s", baseURL, im.Config.Digest),
		token,
		"",
		&cfg,
	); err != nil {
		return nil, fmt.Errorf("config blob: %w", err)
	}

	// 4. 逐层下载
	layersBase := fmt.Sprintf("/tmp/layers/%s/%s", strings.ReplaceAll(repo, "/", "_"), tag)
	var lowerDirs []string
	for _, layer := range im.Layers {
		safeName := strings.Replace(layer.Digest, ":", "_", 1)
		layerDir := filepath.Join(layersBase, safeName)

		// 目录已存在 = 之前下载过，跳过
		if _, err := os.Stat(layerDir); os.IsNotExist(err) {
			fmt.Printf("[pull]  downloading %s (%d MB)...\n",
				layer.Digest[:19], layer.Size/1024/1024)
			if err := extractLayer(baseURL, layer.Digest, token, layerDir); err != nil {
				fmt.Printf("[pull]  WARNING: layer %s unavailable (%v), skipping\n", layer.Digest[:19], err)
				continue // 404 或其他 registry 错误——跳过这一层
			}
		} else {
			fmt.Printf("[pull]  %s (cached)\n", layer.Digest[:19])
		}
		// OCI layers[0] = 基底层，overlay lowerdir 第一个 = 最上层
		// 所以要反转——最新的层放在最前面
		lowerDirs = append([]string{layerDir}, lowerDirs...)
	}

	return &PullResult{
		LowerDirs:  strings.Join(lowerDirs, ":"),
		Env:        cfg.Config.Env,
		Cmd:        cfg.Config.Cmd,
		Entrypoint: cfg.Config.Entrypoint,
	}, nil
}

// extractLayer 下载一个 blob（tar.gz 格式）并解压到 destDir。
// 处理普通文件、目录、符号链接。不处理硬链接（容器镜像很少用）。
func extractLayer(baseURL, digest, token, destDir string) error {
	url := fmt.Sprintf("%s/blobs/%s", baseURL, digest)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}

	// 第一层：gzip 解压
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()

	// 第二层：tar 遍历
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar: %w", err)
		}

		target := filepath.Join(destDir, hdr.Name)

		switch hdr.Typeflag {
		case tar.TypeDir:
			os.MkdirAll(target, 0755)
			os.Chmod(target, os.FileMode(hdr.Mode))
		case tar.TypeReg:
			os.MkdirAll(filepath.Dir(target), 0755)
			f, err := os.Create(target)
			if err != nil {
				return fmt.Errorf("create %s: %w", target, err)
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
			os.Chmod(target, os.FileMode(hdr.Mode))
		case tar.TypeSymlink:
			os.Symlink(hdr.Linkname, target)
		}
	}
	return nil
}
