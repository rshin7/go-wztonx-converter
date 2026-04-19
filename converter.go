package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"runtime"
	"sort"
	"sync"
)

// NX file format constants
const (
	NXMagic      = "PKG4"
	BufferSizeMB = 4 // 4MB buffer for improved write performance
)

// Node types
const (
	NodeTypeNone   = 0
	NodeTypeInt64  = 1
	NodeTypeDouble = 2
	NodeTypeString = 3
	NodeTypePOINT  = 4 // Vector (x, y)
	NodeTypeBitmap = 5
	NodeTypeAudio  = 6
)

// Converter handles the conversion from WZ to NX format
type Converter struct {
	wzFilename string
	nxFilename string
	client     bool
	hc         bool

	// NX data structures
	nodes     []*Node
	nodeIndex map[*Node]uint32
	strings   []string
	stringMap map[string]uint32
	bitmaps   []BitmapData
	audio     []AudioData

	mu sync.Mutex // protects bitmaps, audio, strings during parallel parsing

	// Debug logging
	debugLog *log.Logger
	logFile  *os.File
}

// Node represents a node in the NX file
type Node struct {
	Name     string
	Children []*Node
	Type     uint16
	Data     interface{}
}

// BitmapNodeData stores bitmap node information
type BitmapNodeData struct {
	ID     uint32
	Width  uint16
	Height uint16
}

// AudioNodeData stores audio node information
type AudioNodeData struct {
	ID     uint32
	Length uint32
}

// BitmapData stores bitmap information
type BitmapData struct {
	Width          uint16
	Height         uint16
	Data           []byte
	CompressedData []byte
	Offset         uint64
}

// AudioData stores audio information
type AudioData struct {
	Length         uint32
	Data           []byte
	CompressedData []byte
	Offset         uint64
}

// bufferedSeeker wraps a bufio.Writer to provide both buffered writing and seeking
type bufferedSeeker struct {
	file   *os.File
	writer *bufio.Writer
}

// newBufferedSeeker creates a new buffered seeker with a large buffer
func newBufferedSeeker(file *os.File, bufferSize int) *bufferedSeeker {
	return &bufferedSeeker{
		file:   file,
		writer: bufio.NewWriterSize(file, bufferSize),
	}
}

// Write writes data to the buffer
func (bs *bufferedSeeker) Write(p []byte) (n int, err error) {
	return bs.writer.Write(p)
}

// Seek flushes the buffer and then seeks to the specified position
func (bs *bufferedSeeker) Seek(offset int64, whence int) (int64, error) {
	// Must flush before seeking
	if err := bs.writer.Flush(); err != nil {
		return 0, err
	}
	return bs.file.Seek(offset, whence)
}

// Flush flushes the buffer to the underlying file
func (bs *bufferedSeeker) Flush() error {
	return bs.writer.Flush()
}

// Close flushes and closes the file
func (bs *bufferedSeeker) Close() error {
	if err := bs.writer.Flush(); err != nil {
		return err
	}
	return bs.file.Close()
}

// posTracker wraps an io.Writer and tracks the current write position,
// avoiding costly Seek(0, SeekCurrent) calls that would flush the buffer.
type posTracker struct {
	w   io.Writer
	pos uint64
}

func (p *posTracker) Write(data []byte) (int, error) {
	n, err := p.w.Write(data)
	p.pos += uint64(n)
	return n, err
}

// NewConverter creates a new converter instance
func NewConverter(wzFile, nxFile string, client, hc bool) *Converter {
	return &Converter{
		wzFilename: wzFile,
		nxFilename: nxFile,
		client:     client,
		hc:         hc,
		stringMap:  make(map[string]uint32),
	}
}

// EnableDebugLogging enables debug logging to the specified file
func (c *Converter) EnableDebugLogging(logFilename string) error {
	f, err := os.Create(logFilename)
	if err != nil {
		return err
	}
	c.logFile = f
	c.debugLog = log.New(f, "", log.Ldate|log.Ltime|log.Lmicroseconds)
	c.debugLog.Println("=== Debug logging enabled ===")
	return nil
}

// debugf logs a formatted debug message if debug logging is enabled
func (c *Converter) debugf(format string, args ...interface{}) {
	if c.debugLog != nil {
		c.debugLog.Printf(format, args...)
	}
}

// Convert performs the WZ to NX conversion
func (c *Converter) Convert() error {
	// Close debug log file at the end if it was opened
	if c.logFile != nil {
		defer func() {
			c.debugf("=== Conversion complete ===")
			c.logFile.Close()
		}()
	}

	c.debugf("Starting conversion: %s -> %s", c.wzFilename, c.nxFilename)
	fmt.Print("Parsing input.......")

	// Parse WZ file
	if err := c.parseWZFile(); err != nil {
		return fmt.Errorf("parsing WZ file: %w", err)
	}

	fmt.Println("Done!")
	fmt.Println("Creating output.....")

	// Write NX file
	if err := c.writeNXFile(); err != nil {
		return fmt.Errorf("writing NX file: %w", err)
	}

	fmt.Println("Done!")
	return nil
}

// parseWZFile is implemented in wzparser.go

// writeNXFile writes the NX format file
func (c *Converter) writeNXFile() error {
	file, err := os.Create(c.nxFilename)
	if err != nil {
		return err
	}
	defer file.Close()

	// Create buffered writer with large buffer for improved write performance
	bufferSize := BufferSizeMB * 1024 * 1024
	bufferedWriter := newBufferedSeeker(file, bufferSize)

	// Write NX data using buffered writer
	if err := c.writeNXData(bufferedWriter); err != nil {
		return err
	}

	// Ensure all data is flushed
	return bufferedWriter.Flush()
}

// writeNXData writes the actual NX format data
func (c *Converter) writeNXData(bs *bufferedSeeker) error {
	pt := &posTracker{w: bs, pos: 0}

	fmt.Print("  Writing header...")
	if err := c.writeHeader(pt); err != nil {
		return err
	}
	fmt.Println("Done!")

	nodeOffset := pt.pos
	fmt.Printf("  Writing %d nodes...\n", len(c.nodes))
	if err := c.writeNodes(pt); err != nil {
		return err
	}
	fmt.Println("  Done!")

	fmt.Printf("  Writing %d strings...", len(c.strings))
	stringOffsetTableOffset, err := c.writeStrings(pt)
	if err != nil {
		return err
	}
	fmt.Println("Done!")

	var bitmapOffsetTableOffset uint64
	var audioOffsetTableOffset uint64

	if c.client {
		if len(c.bitmaps) > 0 {
			fmt.Printf("  Compressing %d bitmaps...", len(c.bitmaps))
			if err := c.compressBitmapsParallel(); err != nil {
				return err
			}
			fmt.Println("Done!")

			fmt.Print("  Writing bitmaps...")
			bitmapOffsetTableOffset, err = c.writeBitmaps(pt)
			if err != nil {
				return err
			}
			fmt.Println("Done!")
		}

		if len(c.audio) > 0 {
			fmt.Printf("  Writing %d audio files...", len(c.audio))
			audioOffsetTableOffset, err = c.writeAudio(pt)
			if err != nil {
				return err
			}
			fmt.Println("Done!")
		}
	}

	fmt.Print("  Finalizing header...")
	if err := bs.Flush(); err != nil {
		return err
	}
	if err := c.updateHeader(bs, nodeOffset, stringOffsetTableOffset, bitmapOffsetTableOffset, audioOffsetTableOffset); err != nil {
		return err
	}
	fmt.Println("Done!")

	return nil
}

// writeHeader writes a 52-byte placeholder header (updated at the end).
func (c *Converter) writeHeader(w io.Writer) error {
	var buf [52]byte
	copy(buf[0:4], NXMagic)
	_, err := w.Write(buf[:])
	return err
}

// writeNodes writes all nodes using O(1) child index lookups and direct byte encoding.
func (c *Converter) writeNodes(w io.Writer) error {
	totalNodes := len(c.nodes)
	buf := make([]byte, 20)
	var lastPercent int = -1

	for i, node := range c.nodes {
		nameID := c.getStringID(node.Name)

		var firstChild uint32 = 0
		var childCount uint16 = 0
		if len(node.Children) > 0 {
			firstChild = c.nodeIndex[node.Children[0]]
			childCount = uint16(len(node.Children))
		}

		binary.LittleEndian.PutUint32(buf[0:4], nameID)
		binary.LittleEndian.PutUint32(buf[4:8], firstChild)
		binary.LittleEndian.PutUint16(buf[8:10], childCount)
		binary.LittleEndian.PutUint16(buf[10:12], node.Type)
		c.encodeNodeData(buf[12:20], node)

		if _, err := w.Write(buf); err != nil {
			return err
		}

		percent := (i + 1) * 100 / totalNodes
		if percent != lastPercent {
			fmt.Printf("\r  Progress: %d%%", percent)
			lastPercent = percent
		}
	}

	fmt.Println()
	return nil
}

// encodeNodeData writes 8 bytes of type-specific data directly into buf.
func (c *Converter) encodeNodeData(buf []byte, node *Node) {
	_ = buf[7] // bounds check elimination hint
	buf[0] = 0
	buf[1] = 0
	buf[2] = 0
	buf[3] = 0
	buf[4] = 0
	buf[5] = 0
	buf[6] = 0
	buf[7] = 0

	switch node.Type {
	case NodeTypeInt64:
		if v, ok := node.Data.(int64); ok {
			binary.LittleEndian.PutUint64(buf, uint64(v))
		}
	case NodeTypeDouble:
		if v, ok := node.Data.(float64); ok {
			binary.LittleEndian.PutUint64(buf, math.Float64bits(v))
		}
	case NodeTypeString:
		if s, ok := node.Data.(string); ok {
			binary.LittleEndian.PutUint32(buf[0:4], c.getStringID(s))
		}
	case NodeTypePOINT:
		if p, ok := node.Data.([2]int32); ok {
			binary.LittleEndian.PutUint32(buf[0:4], uint32(p[0]))
			binary.LittleEndian.PutUint32(buf[4:8], uint32(p[1]))
		}
	case NodeTypeBitmap:
		if b, ok := node.Data.(BitmapNodeData); ok {
			binary.LittleEndian.PutUint32(buf[0:4], b.ID)
			binary.LittleEndian.PutUint16(buf[4:6], b.Width)
			binary.LittleEndian.PutUint16(buf[6:8], b.Height)
		}
	case NodeTypeAudio:
		if a, ok := node.Data.(AudioNodeData); ok {
			binary.LittleEndian.PutUint32(buf[0:4], a.ID)
			binary.LittleEndian.PutUint32(buf[4:8], a.Length)
		}
	}
}

// updateHeader rewrites the complete 52-byte header with final values.
func (c *Converter) updateHeader(bs *bufferedSeeker, nodeOffset, stringOffsetTableOffset, bitmapOffsetTableOffset, audioOffsetTableOffset uint64) error {
	if _, err := bs.file.Seek(0, io.SeekStart); err != nil {
		return err
	}

	var buf [52]byte
	copy(buf[0:4], NXMagic)
	binary.LittleEndian.PutUint32(buf[4:8], uint32(len(c.nodes)))
	binary.LittleEndian.PutUint64(buf[8:16], nodeOffset)
	binary.LittleEndian.PutUint32(buf[16:20], uint32(len(c.strings)))
	binary.LittleEndian.PutUint64(buf[20:28], stringOffsetTableOffset)
	binary.LittleEndian.PutUint32(buf[28:32], uint32(len(c.bitmaps)))
	binary.LittleEndian.PutUint64(buf[32:40], bitmapOffsetTableOffset)
	binary.LittleEndian.PutUint32(buf[40:44], uint32(len(c.audio)))
	binary.LittleEndian.PutUint64(buf[44:52], audioOffsetTableOffset)

	_, err := bs.file.Write(buf[:])
	return err
}

// writeStrings writes string data followed by the offset table.
func (c *Converter) writeStrings(pt *posTracker) (uint64, error) {
	stringOffsets := make([]uint64, len(c.strings))

	var lenbuf [2]byte
	for i, str := range c.strings {
		stringOffsets[i] = pt.pos

		binary.LittleEndian.PutUint16(lenbuf[:], uint16(len(str)))
		if _, err := pt.Write(lenbuf[:]); err != nil {
			return 0, err
		}
		if _, err := pt.Write([]byte(str)); err != nil {
			return 0, err
		}
	}

	stringOffsetTableOffset := pt.pos

	var buf [8]byte
	for _, offset := range stringOffsets {
		binary.LittleEndian.PutUint64(buf[:], offset)
		if _, err := pt.Write(buf[:]); err != nil {
			return 0, err
		}
	}

	return stringOffsetTableOffset, nil
}

// writeBitmaps writes bitmap data followed by the offset table.
func (c *Converter) writeBitmaps(pt *posTracker) (uint64, error) {
	bitmapOffsets := make([]uint64, len(c.bitmaps))

	var sizebuf [4]byte
	for i, bitmap := range c.bitmaps {
		bitmapOffsets[i] = pt.pos

		binary.LittleEndian.PutUint32(sizebuf[:], uint32(len(bitmap.CompressedData)))
		if _, err := pt.Write(sizebuf[:]); err != nil {
			return 0, err
		}
		if _, err := pt.Write(bitmap.CompressedData); err != nil {
			return 0, err
		}
	}

	bitmapOffsetTableOffset := pt.pos

	var buf [8]byte
	for _, offset := range bitmapOffsets {
		binary.LittleEndian.PutUint64(buf[:], offset)
		if _, err := pt.Write(buf[:]); err != nil {
			return 0, err
		}
	}

	return bitmapOffsetTableOffset, nil
}

// writeAudio writes audio data followed by the offset table.
func (c *Converter) writeAudio(pt *posTracker) (uint64, error) {
	audioOffsets := make([]uint64, len(c.audio))

	for i := range c.audio {
		audioOffsets[i] = pt.pos

		// Audio data is already in final form (82-byte header + audio payload)
		if len(c.audio[i].CompressedData) == 0 && len(c.audio[i].Data) > 0 {
			c.audio[i].CompressedData = c.audio[i].Data
		}

		if _, err := pt.Write(c.audio[i].CompressedData); err != nil {
			return 0, err
		}
	}

	audioOffsetTableOffset := pt.pos

	var buf [8]byte
	for _, offset := range audioOffsets {
		binary.LittleEndian.PutUint64(buf[:], offset)
		if _, err := pt.Write(buf[:]); err != nil {
			return 0, err
		}
	}

	return audioOffsetTableOffset, nil
}

// addString adds a string to the string table and returns its ID.
// Thread-safe: protected by c.mu for use during parallel WZ parsing.
func (c *Converter) addString(str string) uint32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if id, exists := c.stringMap[str]; exists {
		return id
	}
	id := uint32(len(c.strings))
	c.strings = append(c.strings, str)
	c.stringMap[str] = id
	return id
}

// getStringID returns the ID for a string, adding it if absent.
func (c *Converter) getStringID(str string) uint32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if id, exists := c.stringMap[str]; exists {
		return id
	}
	id := uint32(len(c.strings))
	c.strings = append(c.strings, str)
	c.stringMap[str] = id
	return id
}

// compressBitmapsParallel compresses all bitmap data in parallel.
// Frees raw pixel data after compression to reduce memory usage.
func (c *Converter) compressBitmapsParallel() error {
	if len(c.bitmaps) == 0 {
		return nil
	}

	errChan := make(chan error, len(c.bitmaps))
	var wg sync.WaitGroup

	maxWorkers := runtime.NumCPU() * 2
	if maxWorkers < 16 {
		maxWorkers = 16
	}
	semaphore := make(chan struct{}, maxWorkers)

	for i := range c.bitmaps {
		if len(c.bitmaps[i].CompressedData) > 0 || len(c.bitmaps[i].Data) == 0 {
			continue
		}

		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			compressed, err := c.compressData(c.bitmaps[index].Data)
			if err != nil {
				errChan <- fmt.Errorf("compressing bitmap %d: %w", index, err)
				return
			}
			c.bitmaps[index].CompressedData = compressed
			c.bitmaps[index].Data = nil // free raw pixels to reduce memory
		}(i)
	}

	wg.Wait()
	close(errChan)

	for err := range errChan {
		if err != nil {
			return err
		}
	}

	return nil
}

// flattenNodes performs a BFS to lay out nodes contiguously (parent's children
// at [firstChild, firstChild+count-1]) and builds an index for O(1) lookup.
func (c *Converter) flattenNodes(root *Node) {
	c.nodeIndex = make(map[*Node]uint32, 1024*1024)
	var queue []*Node
	queue = append(queue, root)

	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]

		c.nodeIndex[node] = uint32(len(c.nodes))
		c.nodes = append(c.nodes, node)

		// Sort children alphabetically so binary search in the NX reader works.
		sort.Slice(node.Children, func(i, j int) bool {
			return node.Children[i].Name < node.Children[j].Name
		})

		queue = append(queue, node.Children...)
	}
}
