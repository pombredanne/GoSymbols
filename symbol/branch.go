package symbol

import (
	"bufio"
	"encoding/gob"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/adyzng/GoSymbols/config"
	"github.com/adyzng/GoSymbols/util"

	log "gopkg.in/clog.v1"
)

const (
	adminDir  = "000Admin"
	unzipDir  = "000Unzip"
	lastidTxt = "lastid.txt" // build ID generated by symstore.exe
	serverTxt = "server.txt" // build history generated by symstore.exe
	branchBin = "branch.bin" // current branch information generated by GoSymbols
	d2dNative = "\\D2D\\Native"

	ArchX86 = "x86"
	ArchX64 = "x64"
)

var (
	ErrBuildNotExist       = fmt.Errorf("build not exist")
	ErrBranchNotInit       = fmt.Errorf("branch not initialized")
	ErrBranchOnSymbolStore = fmt.Errorf("invalid branch on symbol store")
	ErrBranchOnBuildServer = fmt.Errorf("invalid branch on build server")
)

// BrBuilder represent pdb release
//
type BrBuilder struct {
	Branch
	builds  map[string]*Build  // save all builds for current branch
	symbols map[string]*Symbol // save symbols
	symPath string             // path that unzip debug.zip to
	mx      sync.RWMutex
}

func init() {
	log.New(log.CONSOLE, log.ConsoleConfig{
		Level:      log.INFO,
		BufferSize: 100,
	})
}

// NewBranch create an new `BrBuilder`, `Init` must be called after `NewBranch`.
//
func NewBranch(buildName, storeName string) *BrBuilder {
	return NewBranch2(&Branch{
		BuildName:  buildName,
		StoreName:  storeName,
		UpdateDate: time.Now().Format("2006-01-02 15:04:05"),
	})
}

// NewBranch2 ...
func NewBranch2(branch *Branch) *BrBuilder {
	b := &BrBuilder{
		Branch:  *branch,
		builds:  make(map[string]*Build, 1),
		symbols: make(map[string]*Symbol, 1),
	}
	if b.StorePath == "" {
		b.StorePath = filepath.Join(config.Destination, b.StoreName)
	}
	if b.BuildPath == "" {
		b.BuildPath = filepath.Join(config.BuildSource, b.BuildName, "Release")
	}
	return b
}

// Name return name in symbol store
//
func (b *BrBuilder) Name() string {
	return b.StoreName
}

// CanBrowse check if current branch is valid on local symbol store.
func (b *BrBuilder) CanBrowse() bool {
	fpath := filepath.Join(b.StorePath, adminDir)
	if st, _ := os.Stat(fpath); st != nil && st.IsDir() {
		return true
	}
	log.Trace("[Branch] Access sympol path %s failed.", fpath)
	return false
}

// CanUpdate check if current branch is valid on build server.
func (b *BrBuilder) CanUpdate() bool {
	fpath := filepath.Join(b.BuildPath, config.LatestBuildFile)
	if st, _ := os.Stat(fpath); st != nil && !st.IsDir() {
		return true
	}
	log.Trace("[Branch] Access build path %s failed.", fpath)
	return false
}

// SetSubpath change the subpath on build server and local store.
// `buildserver` is the subpath relative to config.BuildSource.
// `localstore` is the subpath relative to config.Destination.
//
func (b *BrBuilder) SetSubpath(buildserver, localstore string) error {
	lpath := filepath.Join(config.Destination, b.StoreName)
	fpath := filepath.Join(config.BuildSource, b.BuildName, "Release")

	if localstore != "" {
		// by given subpath
		lpath = filepath.Join(config.Destination, localstore)
	}
	if err := os.MkdirAll(filepath.Join(lpath, adminDir), 666); err != nil {
		log.Error(2, "[Branch] Init sympol store path %s failed: %v.", lpath, err)
		return err
	}
	b.StorePath = lpath

	if buildserver != "" {
		// by given subpath
		fpath = filepath.Join(config.BuildSource, buildserver)
	}
	b.BuildPath = fpath

	// check if can be update from server
	if _, err := os.Stat(fpath); os.IsNotExist(err) {
		log.Error(2, "[Branch] Invalid path %s for %s.", fpath, b.Name())
		return fmt.Errorf("invalid path on build server")
	}
	return nil
}

// Persist will save branch information into 000Admin/branch.bin
//
func (b *BrBuilder) Persist() error {
	fpath := filepath.Join(b.StorePath, adminDir, branchBin)
	fd, err := os.OpenFile(fpath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 666)
	if err != nil {
		log.Error(2, "[Branch] Persist branch %s failed: %v.", b.Name(), err)
		return err
	}

	defer fd.Close()
	log.Trace("[Branch] Save branch %+v.", b.Branch)
	return gob.NewEncoder(fd).Encode(&b.Branch)
}

// Delete current branch
//
func (b *BrBuilder) Delete() error {
	log.Info("[Branch] Delete branch %+v.", b.Branch)
	fpath := filepath.Join(b.StorePath, adminDir, branchBin)
	err := os.Remove(fpath)
	return err
}

// Load will load branch information from 000Admin/branch.bin
//
func (b *BrBuilder) Load() error {
	fpath := filepath.Join(b.StorePath, adminDir, branchBin)
	fd, err := os.OpenFile(fpath, os.O_RDONLY, 666)
	if err != nil {
		//log.Error(2, "[Branch] Load branch %s failed: %v.", b.Name(), err)
		return err
	}

	defer fd.Close()
	return gob.NewDecoder(fd).Decode(&b.Branch)
}

// getSymbols copy pdb zip file to local temp path and return the path
//
func (b *BrBuilder) getSymbols(buildver string) (string, error) {
	var (
		fs    *os.File
		fd    *os.File
		err   error
		bytes int64
	)

	fsrc := fmt.Sprintf("%s\\Build%s\\%s", b.BuildPath, buildver, config.PDBZipFile)
	fzip := filepath.Join(b.symPath, config.PDBZipFile)

	fd, err = os.OpenFile(fzip, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.ModeTemporary)
	if err != nil {
		log.Error(2, "[Branch] create zip file %s failed: %v.", fzip, err)
		return "", err
	}
	defer fd.Close()

	fs, err = os.OpenFile(fsrc, os.O_RDONLY, 666)
	if err != nil {
		log.Error(2, "[Branch] open source file %s failed: %v.", fsrc, err)
		return "", err
	}
	defer fs.Close()

	log.Info("[Branch] Copy %s to %s.", fsrc, fzip)
	start := time.Now()
	bytes, err = io.Copy(fd, fs)
	log.Info("[Branch] Copy complete: Size = %d, Time = %s.", bytes, time.Since(start))

	if err != nil {
		log.Error(2, "[Branch] Copy zip file failed: %v.", fsrc, err)
		return "", err
	}
	return fzip, nil
}

// getLatestBuild return latest build no. on build server
//
func (b *BrBuilder) getLatestBuild(local bool) (string, error) {
	fpath := ""
	if local {
		fpath = filepath.Join(b.StorePath, adminDir, config.LatestBuildFile)
	} else {
		fpath = filepath.Join(b.BuildPath, config.LatestBuildFile)
	}

	fd, err := os.OpenFile(fpath, os.O_RDONLY, 666)
	if err != nil {
		return "", err
	}

	defer fd.Close()
	r := bufio.NewReader(fd)

	str, _ := r.ReadString('\n')
	return strings.Trim(str, " \r\n"), nil
}

// GetLatestID return the last symbol build id
//
func (b *BrBuilder) GetLatestID() string {
	fpath := filepath.Join(b.StorePath, adminDir, lastidTxt)
	fd, err := os.OpenFile(fpath, os.O_RDONLY, 666)
	if err != nil {
		log.Error(2, "[Branch] Read latest build (%s) failed with %v.", fpath, err)
		return ""
	}

	defer fd.Close()
	r := bufio.NewReader(fd)

	str, _ := r.ReadString('\n')
	return strings.Trim(str, " \r\n")
}

// updateLatestBuild update local latest build file
//
func (b *BrBuilder) updateLatestBuild(latest string) error {
	fpath := filepath.Join(b.StorePath, adminDir, config.LatestBuildFile)
	fd, err := os.OpenFile(fpath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 666)
	if err != nil {
		log.Error(2, "[Branch] Open local latest build (%s) failed with %v.", fpath, err)
		return err
	}
	defer fd.Close()

	if _, err = fd.WriteString(latest); err != nil {
		log.Error(2, "[Branch] Write local latest build (%s) failed with %v.", fpath, err)
		return err
	}
	return nil
}

// addSymStore call symstore.exe to add symbols to symbol store.
//
func (b *BrBuilder) addSymStore(latestbuild, symbols string) (*Build, error) {
	start := time.Now()
	comment := start.Format("2006-01-02_15:04:05")
	log.Info("[Branch] Call symbol store command for build %s ...", latestbuild)

	/*
		"C:\Program Files (x86)\Windows Kits\8.1\Debuggers\x86\symstore.exe"
			add
			/r
			/l
			/f \\rmdm-bldvm-l902\CurrentRelease\UDP\UDPMAIN\Intermediate\B%BUILD_NUMBER%\debug\*.pdb
			/s S:\SymbolServer\Titanium
			/t Titanium
			/v %BUILD_NUMBER%
			/c %date:~-10%_%time:~0,8%
	*/
	cmd := exec.Command(config.SymStoreExe, "add", "/r",
		"/f", symbols,
		"/s", b.StorePath,
		"/t", b.Name(),
		"/v", latestbuild,
		"/c", comment)

	var (
		err    error
		output []byte
		done   = make(chan struct{}, 1)
	)
	go func() {
		output, err = cmd.CombinedOutput()
		done <- struct{}{}
	}()

	<-done
	log.Info("[Branch] Symbol store output: %s.", string(output))
	log.Info("[Branch] Symbol store complete: %s.", time.Since(start))

	if err != nil {
		log.Info("[Branch] Symbol store command failed with %s.", err)
		return nil, err
	}
	build := &Build{
		ID:      b.GetLatestID(),
		Date:    start.Format("2006-01-02 15:04:05"),
		Branch:  b.Name(),
		Version: latestbuild,
		Comment: comment,
	}
	return build, nil
}

func (b *BrBuilder) getBuild(version string, id string) *Build {
	b.mx.RLock()
	defer b.mx.RUnlock()
	if version != "" {
		for _, val := range b.builds {
			if val.Version == version {
				return val
			}
		}
	}
	if id != "" {
		if build, ok := b.builds[id]; ok {
			return build
		}
	}
	return nil
}

func (b *BrBuilder) addBuild(build *Build) {
	b.mx.Lock()
	defer b.mx.Unlock()

	b.BuildsCount++
	b.UpdateDate = build.Date
	b.builds[build.ID] = build
}

// AddBuild add new version of pdb
//
func (b *BrBuilder) AddBuild(buildVerion string) error {
	latest := buildVerion
	local, err := b.getLatestBuild(true)

	if buildVerion == "" {
		if latest, err = b.getLatestBuild(false); err != nil {
			log.Error(2, "[Branch] Get server latest build failed: %v.", err)
			return fmt.Errorf("invalid build server latestbuild.txt file")
		}
		if latest == local {
			log.Trace("[Branch] Branch %s already updated to latest %s.", b.Name(), latest)
			return nil
		}
	}
	if b.getBuild(latest, "") != nil {
		log.Warn("[Branch] Symbols for build %s already exist.", latest)
		return nil
	}
	log.Info("[Branch] Add symbols for build %s. Local: %s.", latest, local)

	b.symPath = filepath.Join(b.StorePath, unzipDir)
	if err = os.MkdirAll(b.symPath, 666); err != nil {
		log.Error(2, "[Branch] Create symbol path %s failed with %v.", b.symPath, err)
		return err
	}
	defer os.RemoveAll(b.symPath)

	var symbolZip string
	if symbolZip, err = b.getSymbols(latest); err != nil {
		log.Error(2, "[Branch] Get symbols failed: %v.", err)
		return err
	}
	if err = util.Unzip(symbolZip, b.symPath); err != nil {
		log.Error(2, "[Branch] Unzip symbols failed: %v.", err)
		return err
	}

	var build *Build
	if build, err = b.addSymStore(latest, b.symPath); err != nil {
		log.Error(2, "[Branch] Add to symbol store failed with %v.", err)
		return err
	}
	if err = b.updateLatestBuild(latest); err != nil {
		return err
	}

	b.addBuild(build)
	b.LatestBuild = latest
	return nil
}

// ParseBuilds parse server.txt to get pdb history
//
func (b *BrBuilder) ParseBuilds(handler func(b *Build) error) (int, error) {
	if handler == nil {
		handler = func(bd *Build) error {
			//fmt.Println(bd)
			return nil
		}
	}

	total := 0
	if len(b.builds) != 0 {
		for _, bd := range b.builds {
			if err := handler(bd); err != nil {
				log.Error(2, "[Branch] Parse build(%v) failed: %v.", bd, err)
				return total, err
			}
			total++
		}
		return total, nil
	}

	txtPath := filepath.Join(b.StorePath, adminDir, serverTxt)
	fc, err := os.OpenFile(txtPath, os.O_RDONLY, 666)
	if err != nil {
		log.Error(2, "[Branch] Open file (%s) failed with %v.", txtPath, err)
		return 0, err
	}
	defer fc.Close()

	// clean, will re-calculate it
	b.BuildsCount = 0
	r := bufio.NewReader(fc)
	for {
		str, err := r.ReadString('\n')
		if err == io.EOF {
			break
		}
		str = strings.Trim(str, "\r\n")

		//         0   1    2          3        4          5            6                   7
		//0000000001,add,file,07/04/2017,14:44:14,"UDPv6.5U2","4175.2-538","2017/7/4_14:44:14",
		ss := strings.Split(str, ",")
		if len(ss) < 8 {
			log.Warn("[Branch] Invalid line (%s) in server.txt.", str)
			continue
		}

		dateStr := ss[3] + " " + ss[4]
		dateLoc, err := time.ParseInLocation("01/02/2006 15:04:05", dateStr, time.Local)
		if err != nil {
			log.Warn("[Branch] Parse date failed with %v.", err)
		} else {
			dateStr = dateLoc.Format("2006-01-02 15:04:05")
		}

		build := &Build{
			ID:      ss[0],
			Date:    dateStr,
			Branch:  strings.Trim(ss[5], "\""),
			Version: strings.Trim(ss[6], "\""),
			Comment: strings.Trim(ss[7], "\""),
		}

		total++
		b.addBuild(build)
		b.LatestBuild = build.Version

		if err = handler(build); err != nil {
			return total, err
		}
	}

	return total, nil
}

// ParseSymbols parse 000000001(*) from pdb path
//
func (b *BrBuilder) ParseSymbols(buildID string, handler func(sym *Symbol) error) (int, error) {
	build := b.getBuild("", buildID)
	if build == nil {
		log.Error(2, "[Branch] Build %s not exist for %s.", buildID, b.Name())
		return 0, ErrBuildNotExist
	}

	idPath := filepath.Join(b.StorePath, adminDir, buildID)
	fd, err := os.OpenFile(idPath, os.O_RDONLY, 666)
	if err != nil {
		log.Error(2, "[Branch] Open file (%s) failed with %v.", idPath, err)
		return 0, err
	}
	defer fd.Close()

	if handler == nil {
		handler = func(sym *Symbol) error {
			//fmt.Println(sym)
			return nil
		}
	}
	skipFn := func(name string) bool {
		for _, v := range config.SymExcludeList {
			if strings.ToLower(name) == v {
				return true
			}
		}
		return false
	}
	archDetect := func(sympath string) string {
		x64Caps := []string{"x64", "amd64"}
		sympath = strings.ToLower(sympath)
		for _, cap := range x64Caps {
			if strings.Index(sympath, cap) != -1 {
				return ArchX64
			}
		}
		return ArchX86
	}

	total := 0
	r := bufio.NewReader(fd)
	unqMap := make(map[string]*Symbol, 0)

	for {
		str, err := r.ReadString('\n') //0D 0A
		if err == io.EOF {
			break
		}
		str = strings.Trim(str, "\r\n")

		//
		// "cbt_client.pdb\8E3868FEE1FA4AC8A42D0FACA65E0BE41","S:\script\temp\ExternalLib\RHAPdbfile\cbt_client.pdb"
		ss := strings.Split(str, ",")
		if len(ss) < 2 {
			log.Warn("[Branch] Invalid line (%s) in %s.", str, buildID)
			continue
		}

		pName := strings.Split(strings.Trim(ss[0], "\""), "\\")
		if len(pName) != 2 {
			// invalid format
			continue
		}
		if skipFn(pName[0]) {
			// exclude list
			continue
		}
		if _, ok := unqMap[pName[1]]; ok {
			// deplicate symbol
			continue
		}

		spath := strings.Trim(ss[1], "\"")
		if idx := strings.Index(spath, unzipDir); idx != -1 {
			spath = spath[idx+len(unzipDir):]
		}

		sym := &Symbol{
			Name:    pName[0],
			Hash:    pName[1],
			Path:    spath,
			Arch:    archDetect(spath),
			Version: build.Version,
		}
		if err = handler(sym); err != nil {
			return total, err
		}
		total++
		unqMap[sym.Hash] = sym
	}
	return total, err
}

// GetSymbolPath return symbol's full path
//
func (b *BrBuilder) GetSymbolPath(hash, name string) string {
	return filepath.Join(b.StorePath, name, hash, name)
}
