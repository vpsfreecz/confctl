package generation

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"
)

// CreateFlake constructs a new record without creating files. Its caller owns
// the one build-time sample. A zero-offset local time remains distinct from UTC.
func CreateFlake(root, host, toplevel string, rollback *string, inputs, info Object, at time.Time) (*Build, error) {
	root, err := configurationPath(root)
	if err != nil {
		return nil, err
	}
	_, offset := at.Zone()
	date := Timestamp{seconds: big.NewInt(at.Unix()), fraction: new(big.Rat).SetFrac(big.NewInt(int64(at.Nanosecond())), big.NewInt(1e9)), OffsetSeconds: offset, UTC: at.Location() == time.UTC}
	name := at.Format("2006-01-02--15-04-05")
	kernelPath := toplevel
	if !filepath.IsAbs(kernelPath) {
		kernelPath = root + "/" + kernelPath
	}
	kernel, err := kernelVersion(kernelPath)
	if err != nil {
		return nil, err
	}
	return &Build{Host: host, Name: name, Mode: "flakes", Dir: root + "/.confctl/generations/" + SafeHost(host) + "/" + name, Date: date, Toplevel: toplevel, AutoRollback: rollback, Inputs: inputs, InputsInfo: info, KernelVersion: kernel}, nil
}

type writerOps struct {
	mkdir   func(string) error
	symlink func(string, string) error
	open    func(string) (io.WriteCloser, error)
	rename  func(string, string) error
}

// Writer owns only sequential new-record/current writes. Roots is resolved at
// each source access point, after the record closes; it must be supplied by the
// owning login/configuration adapter, or by a private-root test. No USER/UID or
// production login fallback is selected here.
type Writer struct {
	root   string
	roots  func() (string, error)
	suffix func() (string, error)
	ops    writerOps
}

func NewWriter(configuration string, roots func() (string, error)) (*Writer, error) {
	root, err := configurationPath(configuration)
	if err != nil {
		return nil, err
	}
	if roots == nil {
		roots = func() (string, error) { return "", fmt.Errorf("generation root-directory resolver is missing") }
	}
	return &Writer{root: root, roots: roots, suffix: randomSuffix, ops: writerOps{
		mkdir:   func(path string) error { return os.MkdirAll(path, 0777) },
		symlink: os.Symlink,
		open: func(path string) (io.WriteCloser, error) {
			return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0666)
		},
		rename: os.Rename,
	}}, nil
}

func randomSuffix() (string, error) {
	var value [3]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func (w *Writer) buildDir(build *Build) string {
	return w.root + "/.confctl/generations/" + SafeHost(build.Host) + "/" + build.Name
}

// Save follows Build#save's mutation sequence without cleanup, repair or an
// up-front payload/root/collision preflight. It does not update a BuildList.
func (w *Writer) Save(build *Build) error {
	dir := w.buildDir(build)
	if err := w.ops.mkdir(dir); err != nil {
		return err
	}
	if err := w.ops.symlink(build.Toplevel, dir+"/toplevel"); err != nil {
		return err
	}
	if build.AutoRollback == nil {
		return fmt.Errorf("auto_rollback symlink target is nil")
	}
	if err := w.ops.symlink(*build.AutoRollback, dir+"/auto_rollback"); err != nil {
		return err
	}
	for _, input := range build.Inputs {
		target, ok := input.Value.(string)
		if !ok {
			return fmt.Errorf("input %q symlink target is not a string", input.Name)
		}
		if err := w.ops.symlink(target, dir+"/"+input.Name+".input"); err != nil {
			return err
		}
	}
	file, err := w.ops.open(dir + "/generation.json")
	if err != nil {
		return err
	}
	content, primary := encodeBuildRecord(build)
	if primary == nil {
		var n int
		n, primary = file.Write(content)
		if primary == nil && n != len(content) {
			primary = io.ErrShortWrite
		}
	}
	// Ruby File.open's block ensure closes even after a body error. A close
	// exception supersedes that body exception; it is not a successful save.
	if closeErr := file.Close(); closeErr != nil {
		return closeErr
	}
	if primary != nil {
		return primary
	}
	if err := w.addRoot(RootName(build.Host, build.Name, "toplevel"), dir+"/toplevel"); err != nil {
		return err
	}
	if err := w.addRoot(RootName(build.Host, build.Name, "auto_rollback"), dir+"/auto_rollback"); err != nil {
		return err
	}
	for _, input := range build.Inputs {
		if err := w.addRoot(RootName(build.Host, build.Name, "input."+input.Name), dir+"/"+input.Name+".input"); err != nil {
			return err
		}
	}
	return nil
}

func (w *Writer) addRoot(name, target string) error {
	dir, err := w.roots()
	if err != nil {
		return err
	}
	if err := w.ops.mkdir(dir); err != nil {
		return err
	}
	// GCRoot.add calls dir again while forming File.symlink's destination.
	dir, err = w.roots()
	if err != nil {
		return err
	}
	return w.ops.symlink(target, dir+"/"+name)
}

func persistedDate(date Timestamp) (string, error) {
	if date.seconds == nil || date.fraction == nil || !date.seconds.IsInt64() {
		return "", fmt.Errorf("generation date is not representable for a new record")
	}
	wall := time.Unix(date.seconds.Int64(), 0).In(time.FixedZone("generation", date.OffsetSeconds))
	text := wall.Format("2006-01-02T15:04:05")
	if date.UTC {
		return text + "Z", nil
	}
	sign, offset := "+", date.OffsetSeconds
	if offset < 0 {
		sign, offset = "-", -offset
	}
	return fmt.Sprintf("%s%s%02d:%02d", text, sign, offset/3600, offset%3600/60), nil
}

func encodeBuildRecord(build *Build) ([]byte, error) {
	date, err := persistedDate(build.Date)
	if err != nil {
		return nil, err
	}
	var rollback any
	if build.AutoRollback != nil {
		rollback = *build.AutoRollback
	}
	record := Object{{"mode", "flakes"}, {"date", date}, {"toplevel", build.Toplevel}, {"auto_rollback", rollback}, {"inputs", build.Inputs}, {"inputs_info", build.InputsInfo}}
	var compact bytes.Buffer
	if err := encodeWriterValue(&compact, record, 0); err != nil {
		return nil, err
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, compact.Bytes(), "", "  "); err != nil {
		return nil, err
	}
	pretty.WriteByte('\n')
	return pretty.Bytes(), nil
}

// This is record-local output encoding, not another parser or public format
// service. Object/array order and number tokens remain owned by the R1 model.
func encodeWriterValue(b *bytes.Buffer, value any, depth int) error {
	switch value := value.(type) {
	case Object:
		// json2.21.2's default generator counts even empty collections. The
		// record wrapper participates; this check still occurs after file open.
		if depth >= 100 {
			return fmt.Errorf("generation JSON nesting exceeds 100")
		}
		b.WriteByte('{')
		for i, member := range value {
			if i != 0 {
				b.WriteByte(',')
			}
			if err := encodeWriterValue(b, member.Name, depth+1); err != nil {
				return err
			}
			b.WriteByte(':')
			if err := encodeWriterValue(b, member.Value, depth+1); err != nil {
				return err
			}
		}
		b.WriteByte('}')
		return nil
	case []any:
		if depth >= 100 {
			return fmt.Errorf("generation JSON nesting exceeds 100")
		}
		b.WriteByte('[')
		for i, item := range value {
			if i != 0 {
				b.WriteByte(',')
			}
			if err := encodeWriterValue(b, item, depth+1); err != nil {
				return err
			}
		}
		b.WriteByte(']')
		return nil
	case string:
		if !utf8.ValidString(value) {
			return fmt.Errorf("invalid UTF-8 in generation JSON string")
		}
	case json.Number:
		if string(value) == "" {
			return fmt.Errorf("empty generation JSON number")
		}
	case nil, bool, float64:
	default:
		return fmt.Errorf("unsupported generation JSON value %T", value)
	}
	var scalar bytes.Buffer
	encoder := json.NewEncoder(&scalar)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return err
	}
	encoded := bytes.TrimSuffix(scalar.Bytes(), []byte{'\n'})
	if _, floating := value.(float64); floating && !bytes.ContainsAny(encoded, ".eE") {
		encoded = append(encoded, '.', '0')
	}
	b.Write(encoded)
	return nil
}
