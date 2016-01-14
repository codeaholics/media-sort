package mediasort

import (
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fatih/color"
	"github.com/jpillora/media-sort/search"

	"gopkg.in/fsnotify.v1"
)

//Config is a sorter configuration
type Config struct {
	Targets           []string `type:"args" min:"1"`
	TVDir             string   `help:"tv series base directory"`
	MovieDir          string   `help:"movie base directory"`
	PathConfig        `type:"embedded"`
	Extensions        string        `help:"types of files that should be sorted"`
	Concurrency       int           `help:"search concurrency [warning] setting this too high can cause rate-limiting errors"`
	FileLimit         int           `help:"maximum number of files to search"`
	Recursive         bool          `help:"also search through subdirectories"`
	DryRun            bool          `help:"perform sort but don't actually move any files"`
	SkipHidden        bool          `help:"skip dot files"`
	Overwrite         bool          `help:"overwrites duplicates"`
	OverwriteIfLarger bool          `help:"overwrites duplicates if the new file is larger"`
	Watch             bool          `help:"watch the specified directories for changes and re-sort on change"`
	WatchDelay        time.Duration `help:"delay before next sort after a change"`
}

//fsSort is a media sorter
type fsSort struct {
	Config
	validExts map[string]bool
	sorts     map[string]*fileSort
	dirs      map[string]bool
	stats     struct {
		found, matched, moved int
	}
}

type fileSort struct {
	id     int
	path   string
	info   os.FileInfo
	result *Result
	err    error
}

//FileSystemSort performs a media sort
//against the file system using the provided
//configuration
func FileSystemSort(c Config) error {
	if c.MovieDir == "" {
		c.MovieDir = "."
	}
	if c.TVDir == "" {
		c.TVDir = "."
	}
	if c.Watch && !c.Recursive {
		return errors.New("Recursive mode is required to watch directories")
	}
	if c.Overwrite && c.OverwriteIfLarger {
		return errors.New("Overwrite is already specified, overwrite-if-larger is redundant")
	}
	//init fs sort
	fs := &fsSort{
		Config:    c,
		validExts: map[string]bool{},
	}
	for _, e := range strings.Split(c.Extensions, ",") {
		fs.validExts["."+e] = true
	}
	//sort loop
	for {
		//reset state
		fs.sorts = map[string]*fileSort{}
		fs.dirs = map[string]bool{}
		//look for files
		if err := fs.scan(); err != nil {
			return err
		}
		//ensure we have dirs to watch
		if fs.Watch && len(fs.dirs) == 0 {
			return errors.New("No directories to watch")
		}
		//moment of truth - sort all files!
		if err := fs.sortAllFiles(); err != nil {
			return err
		}
		//watch directories
		if !c.Watch {
			break
		}
		if err := fs.watch(); err != nil {
			return err
		}
	}
	return nil
}

func (fs *fsSort) scan() error {
	//scan targets for media files
	for _, path := range fs.Targets {
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if err = fs.add(path, info); err != nil {
			return err
		}
	}
	//ensure we found something
	if len(fs.sorts) == 0 && (!fs.Watch || len(fs.dirs) == 0) {
		return fmt.Errorf("No sortable files found (%d files checked)", fs.stats.found)
	}
	return nil
}

func (fs *fsSort) sortAllFiles() error {
	//perform sort
	if fs.DryRun {
		log.Println(color.CyanString("[Dryrun]"))
	}
	//sort concurrency-many files at a time,
	//wait for all to complete and keep errors
	queue := make(chan bool, fs.Concurrency)
	wg := &sync.WaitGroup{}
	sortFile := func(file *fileSort) {
		if err := fs.sortFile(file); err != nil {
			log.Printf("[#%d/%d] %s\n  └─> %s\n", file.id, len(fs.sorts), color.RedString(file.path), err)
		}
		<-queue
		wg.Done()
	}
	for _, file := range fs.sorts {
		wg.Add(1)
		queue <- true
		go sortFile(file)
	}
	wg.Wait()
	return nil
}

func (fs *fsSort) watch() error {
	if len(fs.dirs) == 0 {
		return errors.New("No directories to watch")
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("Failed to create file watcher: %s", err)
	}
	for dir, _ := range fs.dirs {
		if err := watcher.Add(dir); err != nil {
			return fmt.Errorf("Failed to watch directory: %s", err)
		}
		log.Printf("Watching %s for changes...", color.CyanString(dir))
	}
	select {
	case <-watcher.Events:
	case <-watcher.Errors:
	}
	go watcher.Close()
	log.Printf("Change detected, re-sorting in %s...", fs.WatchDelay)
	time.Sleep(fs.WatchDelay)
	return nil
}

func (fs *fsSort) add(path string, info os.FileInfo) error {
	//skip hidden files and directories
	if fs.SkipHidden && strings.HasPrefix(info.Name(), ".") {
		return nil
	}
	//limit recursion depth
	if len(fs.sorts) >= fs.FileLimit {
		return nil
	}
	//add regular files (non-symlinks)
	if info.Mode().IsRegular() {
		fs.stats.found++
		if !fs.validExts[filepath.Ext(path)] {
			return nil //skip invalid media file
		}
		fs.sorts[path] = &fileSort{id: len(fs.sorts) + 1, path: path, info: info}
		fs.stats.matched++
		return nil
	}
	//recurse into directories
	if info.IsDir() {
		if !fs.Recursive {
			return errors.New("Recursive mode (-r) is required to sort directories")
		}
		//note directory
		fs.dirs[path] = true
		//add all files in dir
		infos, err := ioutil.ReadDir(path)
		if err != nil {
			return err
		}
		for _, info := range infos {
			p := filepath.Join(path, info.Name())
			//recurse
			if err := fs.add(p, info); err != nil {
				return err
			}
		}
	}
	//skip links,pipes,etc
	return nil
}

func (fs *fsSort) sortFile(file *fileSort) error {
	result, err := Sort(file.path)
	if err != nil {
		return err
	}
	newPath, err := result.PrettyPath(fs.PathConfig)
	if err != nil {
		return err
	}
	baseDir := ""
	switch mediasearch.MediaType(result.MType) {
	case mediasearch.Series:
		baseDir = fs.TVDir
	case mediasearch.Movie:
		baseDir = fs.MovieDir
	default:
		return fmt.Errorf("Invalid result type: %s", result.MType)
	}
	newPath = filepath.Join(baseDir, newPath)
	//DEBUG
	// log.Printf("SUCCESS = D%d #%d\n  %s\n  %s", r.Distance, len(query), query, r.Title)
	log.Printf("[#%d/%d] %s\n  └─> %s", file.id, len(fs.sorts), color.GreenString(result.Path), color.GreenString(newPath))
	if fs.DryRun {
		return nil //dont actually move
	}
	if result.Path == newPath {
		return nil //already sorted
	}

	//check already exists
	if newInfo, err := os.Stat(newPath); err == nil {
		newIsLarger := newInfo.Size() > file.info.Size()
		overwrite := fs.Overwrite
		if !overwrite && fs.OverwriteIfLarger && newIsLarger {
			overwrite = true
		}
		if !overwrite {
			return fmt.Errorf("File already exists '%s' (try setting --overwrite)", newPath)
		}
	}
	//mkdir -p
	err = os.MkdirAll(filepath.Dir(newPath), 0755)
	if err != nil {
		return err //failed to mkdir
	}
	//mv
	err = os.Rename(result.Path, newPath)
	if err != nil {
		return err //failed to move
	}
	//if .srt file exists for the file, mv it too
	pathSubs := strings.TrimSuffix(result.Path, filepath.Ext(result.Path)) + ".srt"
	if _, err := os.Stat(pathSubs); err == nil {
		newPathSubs := strings.TrimSuffix(newPath, filepath.Ext(newPath)) + ".srt"
		os.Rename(pathSubs, newPathSubs) //best-effort
	}
	return nil
}
