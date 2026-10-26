package jshost

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"strings"

	"github.com/dop251/goja"
	"golang.org/x/text/encoding/simplifiedchinese"
)

// convertMap 实现 init.js 中 Convert.* 的所有方法，对应 App 侧
// lib/foundation/js_engine.dart 的 _convert()。
//
// 语义对齐要点：
//   - 入参/出参的二进制一律是 JS ArrayBuffer；
//   - 出错时 App 返回 null（而非抛异常），这里保持一致，让源自行降级；
//   - AES CBC/ECB 要求输入是块大小整数倍（不做自动填充，App 也没做）。
func (h *Host) convertMap(obj map[string]any) goja.Value {
	typ, _ := obj["type"].(string)
	isEncode, _ := obj["isEncode"].(bool)
	value := toBytes(obj["value"])

	fail := func(e error) goja.Value {
		h.opts.Logger("warning", "Convert", fmt.Sprintf("%s failed: %v", typ, e))
		return goja.Null()
	}

	switch typ {
	case "utf8":
		if isEncode {
			return h.bytesToArrayBuffer([]byte(fmt.Sprint(obj["value"])))
		}
		return h.vm.ToValue(string(value))

	case "gbk":
		if isEncode {
			out, err := simplifiedchinese.GBK.NewEncoder().Bytes(value)
			if err != nil {
				return fail(err)
			}
			return h.bytesToArrayBuffer(out)
		}
		out, err := simplifiedchinese.GBK.NewDecoder().Bytes(value)
		if err != nil {
			return fail(err)
		}
		return h.vm.ToValue(string(out))

	case "base64":
		if isEncode {
			return h.vm.ToValue(base64.StdEncoding.EncodeToString(value))
		}
		// 服务端拿到的往往是 HTTP 正文，而不是用户手输的字符串：
		// 上游时不时会在 base64 前面加 UTF-8 BOM（jm 的域名列表就是这样），
		// 严格的 StdEncoding.DecodeString 会直接失败，源随后拿到 null，
		// 报出来的却是 "Unexpected end of JSON input"——离现场十万八千里。
		// 浏览器/Dart 的 atob 会跳过空白，这里把 BOM 一并剔掉。
		out, err := base64.StdEncoding.DecodeString(strings.Trim(stripBOM(string(value)), " \t\r\n"))
		if err != nil {
			// 容错：部分站点使用无填充或 URL-safe 变体
			out, err = decodeBase64Lenient(string(value))
			if err != nil {
				return fail(err)
			}
		}
		return h.bytesToArrayBuffer(out)

	case "md5", "sha1", "sha256", "sha512":
		var hh hash.Hash
		switch typ {
		case "md5":
			hh = md5.New()
		case "sha1":
			hh = sha1.New()
		case "sha256":
			hh = sha256.New()
		case "sha512":
			hh = sha512.New()
		}
		hh.Write(value)
		return h.bytesToArrayBuffer(hh.Sum(nil))

	case "hmac":
		key := toBytes(obj["key"])
		var hh func() hash.Hash
		switch obj["hash"] {
		case "md5":
			hh = md5.New
		case "sha1":
			hh = sha1.New
		case "sha256":
			hh = sha256.New
		case "sha512":
			hh = sha512.New
		default:
			return fail(fmt.Errorf("unsupported hash %v", obj["hash"]))
		}
		mac := hmac.New(hh, key)
		mac.Write(value)
		if obj["isString"] == true {
			return h.vm.ToValue(hex.EncodeToString(mac.Sum(nil)))
		}
		return h.bytesToArrayBuffer(mac.Sum(nil))

	case "aes-ecb", "aes-cbc", "aes-cfb", "aes-ofb":
		key := toBytes(obj["key"])
		block, err := aes.NewCipher(key)
		if err != nil {
			return fail(err)
		}
		var out []byte
		switch typ {
		case "aes-ecb":
			out, err = ecb(block, value, isEncode)
		case "aes-cbc":
			out, err = cbc(block, toBytes(obj["iv"]), value, isEncode)
		case "aes-cfb":
			out, err = cfb(block, toBytes(obj["iv"]), value, isEncode)
		case "aes-ofb":
			out, err = ofb(block, toBytes(obj["iv"]), value, isEncode)
		}
		if err != nil {
			return fail(err)
		}
		return h.bytesToArrayBuffer(out)

	case "rsa":
		if isEncode {
			return goja.Null()
		}
		keyB64 := string(toBytes(obj["key"]))
		if keyB64 == "" {
			keyB64 = fmt.Sprint(obj["key"])
		}
		out, err := rsaDecryptPKCS1(keyB64, value)
		if err != nil {
			return fail(err)
		}
		return h.bytesToArrayBuffer(out)

	case "hexEncode":
		return h.vm.ToValue(hex.EncodeToString(value))

	case "hexDecode":
		out, err := hex.DecodeString(strings.TrimSpace(string(value)))
		if err != nil {
			return fail(err)
		}
		return h.bytesToArrayBuffer(out)
	}
	return goja.Null()
}

// ecb 手写 ECB（Go 标准库不提供）。源依赖它做简单加解密。
func ecb(block cipher.Block, src []byte, encrypt bool) ([]byte, error) {
	bs := block.BlockSize()
	if len(src) == 0 || len(src)%bs != 0 {
		return nil, fmt.Errorf("ecb: input length %d is not a multiple of %d", len(src), bs)
	}
	dst := make([]byte, len(src))
	for i := 0; i < len(src); i += bs {
		if encrypt {
			block.Encrypt(dst[i:i+bs], src[i:i+bs])
		} else {
			block.Decrypt(dst[i:i+bs], src[i:i+bs])
		}
	}
	return dst, nil
}

func cbc(block cipher.Block, iv, src []byte, encrypt bool) ([]byte, error) {
	if len(iv) != block.BlockSize() {
		return nil, fmt.Errorf("cbc: iv length %d, want %d", len(iv), block.BlockSize())
	}
	if len(src) == 0 || len(src)%block.BlockSize() != 0 {
		return nil, fmt.Errorf("cbc: input length %d not a multiple of %d", len(src), block.BlockSize())
	}
	dst := make([]byte, len(src))
	if encrypt {
		cipher.NewCBCEncrypter(block, iv).CryptBlocks(dst, src)
	} else {
		cipher.NewCBCDecrypter(block, iv).CryptBlocks(dst, src)
	}
	return dst, nil
}

func cfb(block cipher.Block, iv, src []byte, encrypt bool) ([]byte, error) {
	if len(iv) != block.BlockSize() {
		return nil, fmt.Errorf("cfb: iv length %d, want %d", len(iv), block.BlockSize())
	}
	dst := make([]byte, len(src))
	if encrypt {
		cipher.NewCFBEncrypter(block, iv).XORKeyStream(dst, src)
	} else {
		cipher.NewCFBDecrypter(block, iv).XORKeyStream(dst, src)
	}
	return dst, nil
}

func ofb(block cipher.Block, iv, src []byte, encrypt bool) ([]byte, error) {
	if len(iv) != block.BlockSize() {
		return nil, fmt.Errorf("ofb: iv length %d, want %d", len(iv), block.BlockSize())
	}
	dst := make([]byte, len(src))
	cipher.NewOFB(block, iv).XORKeyStream(dst, src) // OFB 加解密同构
	return dst, nil
}

// rsaDecryptPKCS1 支持 PKCS#1 / PKCS#8 / PEM 三种私钥表示，
// 并按模长分块解密（与 App 的 _processInBlocks 一致）。
func rsaDecryptPKCS1(keyB64 string, data []byte) ([]byte, error) {
	der, err := base64.StdEncoding.DecodeString(strings.TrimSpace(keyB64))
	if err != nil {
		der, err = decodePEM(keyB64)
		if err != nil {
			return nil, fmt.Errorf("rsa: cannot decode key: %w", err)
		}
	}

	var priv *rsa.PrivateKey
	if k, e := x509.ParsePKCS1PrivateKey(der); e == nil {
		priv = k
	} else if k2, e2 := x509.ParsePKCS8PrivateKey(der); e2 == nil {
		rk, ok := k2.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("rsa: key is not RSA")
		}
		priv = rk
	} else {
		return nil, fmt.Errorf("rsa: parse key: %v / %v", e, e2)
	}

	k := priv.Size()
	var out bytes.Buffer
	for i := 0; i < len(data); i += k {
		end := i + k
		if end > len(data) {
			end = len(data)
		}
		plain, err := rsa.DecryptPKCS1v15(rand.Reader, priv, data[i:end])
		if err != nil {
			return nil, err
		}
		out.Write(plain)
	}
	return out.Bytes(), nil
}

func decodePEM(s string) ([]byte, error) {
	start := strings.Index(s, "-----BEGIN")
	if start < 0 {
		return nil, fmt.Errorf("no PEM block")
	}
	rest := s[start:]
	nl := strings.Index(rest, "\n")
	if nl < 0 {
		return nil, fmt.Errorf("malformed PEM")
	}
	body := rest[nl+1:]
	end := strings.Index(body, "-----END")
	if end < 0 {
		return nil, fmt.Errorf("no PEM end marker")
	}
	body = strings.NewReplacer("\n", "", "\r", "").Replace(body[:end])
	return base64.StdEncoding.DecodeString(strings.TrimSpace(body))
}

// toBytes 把 goja 侧的各种二进制表示统一成 []byte。
func toBytes(v any) []byte {
	switch x := v.(type) {
	case nil:
		return nil
	case []byte:
		return x
	case goja.ArrayBuffer:
		return x.Bytes()
	case string:
		return []byte(x)
	case []any:
		out := make([]byte, len(x))
		for i, e := range x {
			out[i] = byte(toInt(e))
		}
		return out
	case goja.Value:
		if goja.IsUndefined(x) || goja.IsNull(x) {
			return nil
		}
		return toBytes(x.Export())
	case int64:
		return []byte{byte(x)}
	case float64:
		return []byte{byte(int(x))}
	case int:
		return []byte{byte(x)}
	}
	b, _ := json.Marshal(v)
	return b
}

func toInt(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case float64:
		return int64(x)
	case int:
		return int64(x)
	case json.Number:
		n, _ := x.Int64()
		return n
	case string:
		var n int64
		fmt.Sscan(x, &n)
		return n
	case bool:
		if x {
			return 1
		}
		return 0
	case goja.Value:
		if goja.IsUndefined(x) || goja.IsNull(x) {
			return 0
		}
		return toInt(x.Export())
	}
	return 0
}

// bytesToArrayBuffer 把 Go 字节切片转换成 JS 的 ArrayBuffer。
func (h *Host) bytesToArrayBuffer(b []byte) goja.Value {
	if b == nil {
		b = []byte{}
	}
	return h.vm.ToValue(h.vm.NewArrayBuffer(b))
}

func base64Std(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// stripBOM 移除 UTF-8 BOM。上游返回的文本里带 U+FEFF 是常见现象
// （尤其 CDN 回源时），它会让 base64/JSON 解析同时失败。
func stripBOM(s string) string {
	return strings.TrimPrefix(s, "\ufeff")
}

// decodeBase64Lenient 兼容无填充 / URL-safe 的 base64。
func decodeBase64Lenient(s string) ([]byte, error) {
	s = strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\r', '\n', '\ufeff':
			return -1
		}
		return r
	}, s)
	s = strings.NewReplacer("-", "+", "_", "/").Replace(s)
	if m := len(s) % 4; m != 0 {
		s += strings.Repeat("=", 4-m)
	}
	return base64.StdEncoding.DecodeString(s)
}
