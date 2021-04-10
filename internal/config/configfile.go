package config

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hairyhenderson/gomplate/v3/internal/iohelpers"
	"github.com/spf13/afero"
	"gopkg.in/yaml.v3"
)

// Parse a config file
func Parse(in io.Reader) (*Config, error) {
	out := &Config{}
	dec := yaml.NewDecoder(in)
	err := dec.Decode(out)
	if err != nil && err != io.EOF {
		return out, err
	}
	return out, nil
}

// Config - configures the gomplate execution
//
//nolint:govet
type Config struct {
	Input       string   `yaml:"in,omitempty"`
	InputDir    string   `yaml:"inputDir,omitempty"`
	InputFiles  []string `yaml:"inputFiles,omitempty,flow"`
	ExcludeGlob []string `yaml:"excludes,omitempty"`

	OutputDir   string   `yaml:"outputDir,omitempty"`
	OutputMap   string   `yaml:"outputMap,omitempty"`
	OutputFiles []string `yaml:"outputFiles,omitempty,flow"`
	OutMode     string   `yaml:"chmod,omitempty"`

	LDelim string `yaml:"leftDelim,omitempty"`
	RDelim string `yaml:"rightDelim,omitempty"`

	Templates   Templates             `yaml:"templates,omitempty"`
	DataSources map[string]DataSource `yaml:"datasources,omitempty"`
	Context     map[string]DataSource `yaml:"context,omitempty"`
	Plugins     map[string]string     `yaml:"plugins,omitempty"`

	// Extra HTTP headers not attached to pre-defined datsources. Potentially
	// used by datasources defined in the template.
	ExtraHeaders map[string]http.Header `yaml:"-"`

	PostExec []string `yaml:"postExec,omitempty,flow"`

	// internal use only, can't be injected in YAML
	PostExecInput io.Reader `yaml:"-"`

	Stdout io.Writer `yaml:"-"`
	Stderr io.Writer `yaml:"-"`
	Stdin  io.Reader `yaml:"-"`

	PluginTimeout time.Duration `yaml:"pluginTimeout,omitempty"`

	ExecPipe      bool `yaml:"execPipe,omitempty"`
	SuppressEmpty bool `yaml:"suppressEmpty,omitempty"`
	Experimental  bool `yaml:"experimental,omitempty"`

	fs afero.Fs
}

var cfgContextKey = struct{}{}

// ContextWithConfig returns a new context with a reference to the config.
func ContextWithConfig(ctx context.Context, cfg *Config) context.Context {
	return context.WithValue(ctx, cfgContextKey, cfg)
}

// FromContext returns a config from the given context, if any. If no
// config is present a new default configuration will be returned.
func FromContext(ctx context.Context) (cfg *Config) {
	ok := ctx != nil
	if ok {
		cfg, ok = ctx.Value(cfgContextKey).(*Config)
	}
	if !ok {
		cfg = &Config{}
		cfg.ApplyDefaults()
	}
	return cfg
}

// mergeDataSources - use d as defaults, and override with values from o
func mergeDataSources(d, o map[string]DataSource) map[string]DataSource {
	for k, v := range o {
		c, ok := d[k]
		if ok {
			d[k] = c.mergeFrom(v)
		} else {
			d[k] = v
		}
	}
	return d
}

// mergeTemplates - use d as defaults, and override with values from o
func mergeTemplates(d, o Templates) Templates {
	if d == nil {
		return o
	}
	if o == nil {
		return d
	}

	for k, v := range o {
		c, ok := d[k]
		if ok {
			d[k] = c.mergeFrom(v)
		} else {
			d[k] = v
		}
	}
	return d
}

// Templates - mapping type for templates, can be unmarshaled from an array or map
type Templates map[string]DataSource

// UnmarshalYAML - satisfy the yaml.Umarshaler interface - Templates can be
// provided as an array of key=value strings, or a map of string to datasources
func (d *Templates) UnmarshalYAML(value *yaml.Node) error {
	*d = Templates{}

	switch value.Kind {
	case yaml.SequenceNode:
		for _, item := range value.Content {
			parts := strings.SplitN(item.Value, "=", 2)
			ds := DataSource{}
			alias := parts[0]

			u, err := ParseSourceURL(parts[len(parts)-1])
			if err != nil {
				return fmt.Errorf("could not parse datasource URL from %+v: %w", parts, err)
			}

			ds.URL = u
			(*d)[alias] = ds
		}
	case yaml.MappingNode:
		m := map[string]DataSource{}
		err := value.Decode(&m)
		if err != nil {
			return fmt.Errorf("failed to %s node: %w", value.Tag, err)
		}
		for k, v := range m {
			(*d)[k] = v
		}
	default:
		return fmt.Errorf("cannot unmarshal unexpected type %s", value.Tag)
	}

	return nil
}

// DataSource - datasource configuration
type DataSource struct {
	URL    *url.URL    `yaml:"-"`
	Header http.Header `yaml:"header,omitempty,flow"`
}

// UnmarshalYAML - satisfy the yaml.Umarshaler interface - URLs aren't
// well supported, and anyway we need to do some extra parsing
func (d *DataSource) UnmarshalYAML(value *yaml.Node) error {
	type raw struct {
		Header http.Header
		URL    string
	}
	r := raw{}
	err := value.Decode(&r)
	if err != nil {
		return err
	}

	u, err := ParseSourceURL(r.URL)
	if err != nil {
		return fmt.Errorf("could not parse datasource URL %q: %w", r.URL, err)
	}
	*d = DataSource{
		URL:    u,
		Header: r.Header,
	}
	return nil
}

// MarshalYAML - satisfy the yaml.Marshaler interface - URLs aren't
// well supported, and anyway we need to do some extra parsing
func (d DataSource) MarshalYAML() (interface{}, error) {
	type raw struct {
		Header http.Header `yaml:"header,omitempty,flow"`
		URL    string
	}

	r := raw{
		URL:    d.URL.String(),
		Header: d.Header,
	}
	return r, nil
}

// mergeFrom - use this as default, and override with values from o
func (d DataSource) mergeFrom(o DataSource) DataSource {
	if o.URL != nil {
		d.URL = o.URL
	}
	if d.Header == nil {
		d.Header = o.Header
	} else {
		for k, v := range o.Header {
			d.Header[k] = v
		}
	}
	return d
}

// MergeFrom - use this Config as the defaults, and override it with any
// non-zero values from the other Config
//
// Note that Input/InputDir/InputFiles will override each other, as well as
// OutputDir/OutputFiles.
func (c *Config) MergeFrom(o *Config) *Config {
	switch {
	case !isZero(o.Input):
		c.Input = o.Input
		c.InputDir = ""
		c.InputFiles = nil
		c.OutputDir = ""
	case !isZero(o.InputDir):
		c.Input = ""
		c.InputDir = o.InputDir
		c.InputFiles = nil
	case !isZero(o.InputFiles):
		if !(len(o.InputFiles) == 1 && o.InputFiles[0] == "-") {
			c.Input = ""
			c.InputFiles = o.InputFiles
			c.InputDir = ""
			c.OutputDir = ""
		}
	}

	if !isZero(o.OutputMap) {
		c.OutputDir = ""
		c.OutputFiles = nil
		c.OutputMap = o.OutputMap
	}
	if !isZero(o.OutputDir) {
		c.OutputDir = o.OutputDir
		c.OutputFiles = nil
		c.OutputMap = ""
	}
	if !isZero(o.OutputFiles) {
		c.OutputDir = ""
		c.OutputFiles = o.OutputFiles
		c.OutputMap = ""
	}
	if !isZero(o.ExecPipe) {
		c.ExecPipe = o.ExecPipe
		c.PostExec = o.PostExec
		c.OutputFiles = o.OutputFiles
	}
	if !isZero(o.ExcludeGlob) {
		c.ExcludeGlob = o.ExcludeGlob
	}
	if !isZero(o.OutMode) {
		c.OutMode = o.OutMode
	}
	if !isZero(o.LDelim) {
		c.LDelim = o.LDelim
	}
	if !isZero(o.RDelim) {
		c.RDelim = o.RDelim
	}
	mergeDataSources(c.DataSources, o.DataSources)
	mergeDataSources(c.Context, o.Context)
	mergeTemplates(c.Templates, o.Templates)
	if len(o.Plugins) > 0 {
		for k, v := range o.Plugins {
			c.Plugins[k] = v
		}
	}

	return c
}

// ParseDataSourceFlags - sets the DataSources, Context, and Templates fields
// from the key=value format flags as provided at the command-line
func (c *Config) ParseDataSourceFlags(datasources, contexts, templates, headers []string) error {
	for _, d := range datasources {
		k, ds, err := parseDatasourceArg(d)
		if err != nil {
			return err
		}
		if c.DataSources == nil {
			c.DataSources = map[string]DataSource{}
		}
		c.DataSources[k] = ds
	}
	for _, d := range contexts {
		k, ds, err := parseDatasourceArg(d)
		if err != nil {
			return err
		}
		if c.Context == nil {
			c.Context = map[string]DataSource{}
		}
		c.Context[k] = ds
	}

	fsys := c.fs
	if c.fs == nil {
		// if c.fs is unset, don't persist this...
		fsys = afero.NewOsFs()
	}

	parsed, err := parseTemplateArgs(fsys, templates)
	if err != nil {
		return fmt.Errorf("failed to parse template args for %v: %w", templates, err)
	}
	c.Templates = parsed

	hdrs, err := parseHeaderArgs(headers)
	if err != nil {
		return err
	}

	for k, v := range hdrs {
		if d, ok := c.Context[k]; ok {
			d.Header = v
			c.Context[k] = d
			delete(hdrs, k)
		}
		if d, ok := c.DataSources[k]; ok {
			d.Header = v
			c.DataSources[k] = d
			delete(hdrs, k)
		}
		if d, ok := c.Templates[k]; ok {
			d.Header = v
			c.Templates[k] = d
			delete(hdrs, k)
		}
	}
	if len(hdrs) > 0 {
		c.ExtraHeaders = hdrs
	}
	return nil
}

// ParsePluginFlags - sets the Plugins field from the
// key=value format flags as provided at the command-line
func (c *Config) ParsePluginFlags(plugins []string) error {
	for _, plugin := range plugins {
		parts := strings.SplitN(plugin, "=", 2)
		if len(parts) < 2 {
			return fmt.Errorf("plugin requires both name and path")
		}
		if c.Plugins == nil {
			c.Plugins = map[string]string{}
		}
		c.Plugins[parts[0]] = parts[1]
	}
	return nil
}

// ///////////////////////

func parseTemplateArgs(fsys afero.Fs, args []string) (Templates, error) {
	var templates Templates
	for _, arg := range args {
		t, err := parseTemplateArg(fsys, arg)
		if err != nil {
			return nil, err
		}
		templates = mergeTemplates(templates, t)
	}
	return templates, nil
}

// parseTemplateArg - like parseDatasourceArg, except single-part names are
// aliased by the full filename with extension
func parseTemplateArg(fsys afero.Fs, arg string) (Templates, error) {
	templates := Templates{}
	parts := strings.SplitN(arg, "=", 2)
	pth := parts[0]
	key := pth
	if len(parts) > 1 {
		pth = parts[1]
	}

	wasAbsolute := false
	pth = filepath.ToSlash(pth)
	u, err := url.Parse(pth)
	if err == nil && u.IsAbs() {
		wasAbsolute = true
	}

	u, err = ParseSourceURL(pth)
	if err != nil {
		return templates, fmt.Errorf("failed to parse URL for %q: %w", pth, err)
	}

	if u.Scheme != "file" {
		templates[key] = DataSource{URL: u}
		return templates, nil
	}

	if wasAbsolute {
		pth = u.Path
	}

	if !strings.HasSuffix(pth, "/") {
		pth = filepath.Clean(pth)

		//nolint:govet
		f, err := fsys.Open(pth)
		if err != nil {
			return nil, fmt.Errorf("failed to open %q: (%T) %w", pth, err, err)
		}

		fi, err := f.Stat()
		if err != nil {
			return nil, fmt.Errorf("failed to stat %q: %w", f, err)
		}

		if !fi.IsDir() {
			templates[key] = DataSource{URL: u}
			return templates, nil
		}
	}

	pth = filepath.Clean(pth)
	files, err := afero.ReadDir(fsys, pth)
	if err != nil {
		return nil, fmt.Errorf("failed to readDir %q: %w", pth, err)
	}

	// track all of the files within this directory, but no recursion
	for _, f := range files {
		if !f.IsDir() {
			u, err = ParseSourceURL(path.Join(pth, f.Name()))
			if err != nil {
				return templates, err
			}
			templates[path.Join(key, f.Name())] = DataSource{URL: u}
		}
	}

	return templates, nil
}

// ///////////////////////

func parseDatasourceArg(value string) (key string, ds DataSource, err error) {
	parts := strings.SplitN(value, "=", 2)
	if len(parts) == 1 {
		f := parts[0]
		key = strings.SplitN(value, ".", 2)[0]
		if path.Base(f) != f {
			err = fmt.Errorf("invalid datasource (%s): must provide an alias with files not in working directory", value)
			return key, ds, err
		}
		ds.URL, err = ParseSourceURL(f)
	} else if len(parts) == 2 {
		key = parts[0]
		ds.URL, err = ParseSourceURL(parts[1])
	}
	return key, ds, err
}

func parseHeaderArgs(headerArgs []string) (map[string]http.Header, error) {
	headers := make(map[string]http.Header)
	for _, v := range headerArgs {
		ds, name, value, err := splitHeaderArg(v)
		if err != nil {
			return nil, err
		}
		if _, ok := headers[ds]; !ok {
			headers[ds] = make(http.Header)
		}
		headers[ds][name] = append(headers[ds][name], strings.TrimSpace(value))
	}
	return headers, nil
}

func splitHeaderArg(arg string) (datasourceAlias, name, value string, err error) {
	parts := strings.SplitN(arg, "=", 2)
	if len(parts) != 2 {
		err = fmt.Errorf("invalid datasource-header option '%s'", arg)
		return "", "", "", err
	}
	datasourceAlias = parts[0]
	name, value, err = splitHeader(parts[1])
	return datasourceAlias, name, value, err
}

func splitHeader(header string) (name, value string, err error) {
	parts := strings.SplitN(header, ":", 2)
	if len(parts) != 2 {
		err = fmt.Errorf("invalid HTTP Header format '%s'", header)
		return "", "", err
	}
	name = http.CanonicalHeaderKey(parts[0])
	value = parts[1]
	return name, value, nil
}

// Validate the Config
func (c Config) Validate() (err error) {
	err = notTogether(
		[]string{"in", "inputFiles", "inputDir"},
		c.Input, c.InputFiles, c.InputDir)
	if err == nil {
		err = notTogether(
			[]string{"outputFiles", "outputDir", "outputMap"},
			c.OutputFiles, c.OutputDir, c.OutputMap)
	}
	if err == nil {
		err = notTogether(
			[]string{"outputDir", "outputMap", "execPipe"},
			c.OutputDir, c.OutputMap, c.ExecPipe)
	}

	if err == nil {
		err = mustTogether("outputDir", "inputDir",
			c.OutputDir, c.InputDir)
	}

	if err == nil {
		err = mustTogether("outputMap", "inputDir",
			c.OutputMap, c.InputDir)
	}

	if err == nil {
		f := len(c.InputFiles)
		if f == 0 && c.Input != "" {
			f = 1
		}
		o := len(c.OutputFiles)
		if f != o && !c.ExecPipe {
			err = fmt.Errorf("must provide same number of 'outputFiles' (%d) as 'in' or 'inputFiles' (%d) options", o, f)
		}
	}

	if err == nil {
		if c.ExecPipe && len(c.PostExec) == 0 {
			err = fmt.Errorf("execPipe may only be used with a postExec command")
		}
	}

	if err == nil {
		if c.ExecPipe && (len(c.OutputFiles) > 0 && c.OutputFiles[0] != "-") {
			err = fmt.Errorf("must not set 'outputFiles' when using 'execPipe'")
		}
	}

	return err
}

func notTogether(names []string, values ...interface{}) error {
	found := ""
	for i, value := range values {
		if isZero(value) {
			continue
		}
		if found != "" {
			return fmt.Errorf("only one of these options is supported at a time: '%s', '%s'",
				found, names[i])
		}
		found = names[i]
	}
	return nil
}

func mustTogether(left, right string, lValue, rValue interface{}) error {
	if !isZero(lValue) && isZero(rValue) {
		return fmt.Errorf("these options must be set together: '%s', '%s'",
			left, right)
	}

	return nil
}

func isZero(value interface{}) bool {
	switch v := value.(type) {
	case string:
		return v == ""
	case []string:
		return len(v) == 0
	case bool:
		return !v
	default:
		return false
	}
}

// ApplyDefaults - any defaults changed here should be added to cmd.InitFlags as
// well for proper help/usage display.
func (c *Config) ApplyDefaults() {
	if c.InputDir != "" && c.OutputDir == "" && c.OutputMap == "" {
		c.OutputDir = "."
	}
	if c.Input == "" && c.InputDir == "" && len(c.InputFiles) == 0 {
		c.InputFiles = []string{"-"}
	}
	if c.OutputDir == "" && c.OutputMap == "" && len(c.OutputFiles) == 0 && !c.ExecPipe {
		c.OutputFiles = []string{"-"}
	}
	if c.LDelim == "" {
		c.LDelim = "{{"
	}
	if c.RDelim == "" {
		c.RDelim = "}}"
	}

	if c.ExecPipe {
		pipe := &bytes.Buffer{}
		c.PostExecInput = pipe
		c.OutputFiles = []string{"-"}

		// --exec-pipe redirects standard out to the out pipe
		c.Stdout = pipe
	} else {
		c.PostExecInput = c.Stdin
	}

	if c.PluginTimeout == 0 {
		c.PluginTimeout = 5 * time.Second
	}
}

// GetMode - parse an os.FileMode out of the string, and let us know if it's an override or not...
func (c *Config) GetMode() (os.FileMode, bool, error) {
	modeOverride := c.OutMode != ""
	m, err := strconv.ParseUint("0"+c.OutMode, 8, 32)
	if err != nil {
		return 0, false, err
	}
	mode := iohelpers.NormalizeFileMode(os.FileMode(m))
	if mode == 0 && c.Input != "" {
		mode = iohelpers.NormalizeFileMode(0644)
	}
	return mode, modeOverride, nil
}

// String -
func (c *Config) String() string {
	out := &strings.Builder{}
	out.WriteString("---\n")
	enc := yaml.NewEncoder(out)
	enc.SetIndent(2)

	// dereferenced copy so we can truncate input for display
	c2 := *c
	if len(c2.Input) >= 11 {
		c2.Input = c2.Input[0:8] + "..."
	}

	err := enc.Encode(c2)
	if err != nil {
		return err.Error()
	}
	return out.String()
}

// ParseSourceURL parses a datasource URL value, which may be '-' (for stdin://),
// or it may be a Windows path (with driver letter and back-slack separators) or
// UNC, or it may be relative. It also might just be a regular absolute URL...
// In all cases it returns a correct URL for the value.
func ParseSourceURL(value string) (*url.URL, error) {
	if value == "-" {
		value = "stdin://"
	}
	value = filepath.ToSlash(value)
	// handle absolute Windows paths
	volName := ""
	if volName = filepath.VolumeName(value); volName != "" {
		// handle UNCs
		if len(volName) > 2 {
			value = "file:" + value
		} else {
			value = "file:///" + value
		}
	}
	srcURL, err := url.Parse(value)
	if err != nil {
		return nil, err
	}

	if volName != "" && len(srcURL.Path) >= 3 {
		if srcURL.Path[0] == '/' && srcURL.Path[2] == ':' {
			srcURL.Path = srcURL.Path[1:]
		}
	}

	if !srcURL.IsAbs() {
		srcURL, err = absFileURL(value)
		if err != nil {
			return nil, err
		}
	}
	return srcURL, nil
}

func absFileURL(value string) (*url.URL, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("can't get working directory: %w", err)
	}
	wd = filepath.ToSlash(wd)
	baseURL := &url.URL{
		Scheme: "file",
		Path:   filepath.ToSlash(filepath.Clean(wd)) + "/",
	}
	relURL, err := url.Parse(value)
	if err != nil {
		return nil, fmt.Errorf("can't parse value %s as URL: %w", value, err)
	}
	resolved := baseURL.ResolveReference(relURL)
	// deal with Windows drive letters
	if !strings.HasPrefix(wd, "/") && resolved.Path[2] == ':' {
		resolved.Path = resolved.Path[1:]
	}
	return resolved, nil
}
