package caskdb

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"
)

// defaultWhence helps us with `file.Seek` method to move our cursor to certain byte offset for read
// or write operations. The method takes two parameters file.Seek(offset, whence).
// The offset says the byte offset and whence says the direction:
//
// whence 0 - beginning of the file
// whence 1 - current cursor position
// whence 2 - end of the file
//
// read more about it here:
// https://pkg.go.dev/os#File.Seek
const defaultWhence = 0

// MaxFileSize
// while writing to file if max file size reached, new file will be created
var MaxFileSize int64 = 1 << 30

// DiskStore is a Log-Structured Hash Table as described in the BitCask paper. We
// keep appending the data to a file, like a log. DiskStorage maintains an in-memory
// hash table called KeyDir, which keeps the row's location on the disk.
//
// The idea is simple yet brilliant:
//   - Write the record to the disk
//   - Update the internal hash table to point to that byte offset
//   - Whenever we get a read request, check the internal hash table for the address,
//     fetch that and return
//
// KeyDir does not store values, only their locations.
//
// The above approach solves a lot of problems:
//   - Writes are insanely fast since you are just appending to the file
//   - Reads are insanely fast since you do only one disk seek. In B-Tree backed
//     storage, there could be 2-3 disk seeks
//
// However, there are drawbacks too:
//   - We need to maintain an in-memory hash table KeyDir. A database with a large
//     number of keys would require more RAM
//   - Since we need to build the KeyDir at initialisation, it will affect the startup
//     time too
//   - Deleted keys need to be purged from the file to reduce the file size
//
// Read the paper for more details: https://riak.com/assets/bitcask-intro.pdf
//
// DiskStore provides two simple operations to get and set key value pairs. Both key
// and value need to be of string type, and all the data is persisted to disk.
// During startup, DiskStorage loads all the existing KV pair metadata, and it will
// throw an error if the file is invalid or corrupt.
//
// Note that if the database file is large, the initialisation will take time
// accordingly. The initialisation is also a blocking operation; till it is completed,
// we cannot use the database.
//
// Typical usage example:
//
//		store, _ := NewDiskStore("books.db")
//	   	store.Set("othello", "shakespeare")
//	   	author := store.Get("othello")
type DiskStore struct {
	// directory name that contains all data files
	dir string
	// file object pointing the file_name
	file *os.File
	// current cursor position in the file where the data can be written
	writePosition int
	// keyDir is a map of key and KeyEntry being the value. KeyEntry contains the position
	// of the byte offset in the file where the value exists. key_dir map acts as in-memory
	// index to fetch the values quickly from the disk
	keyDir map[string]KeyEntry
}

func isFileExists(fileName string) bool {
	// https://stackoverflow.com/a/12518877
	if _, err := os.Stat(fileName); err == nil || errors.Is(err, fs.ErrExist) {
		return true
	}
	return false
}

func NewDiskStore(directoryName string) (*DiskStore, error) {
	if !isFileExists(directoryName) {
		err := os.MkdirAll(directoryName, os.ModePerm)

		if err != nil {
			return nil, err
		}
	}

	ds := &DiskStore{
		dir:    directoryName,
		keyDir: make(map[string]KeyEntry),
	}

	err := ds.initKeyDir(directoryName)

	if err != nil {
		log.Fatalf("error while loading the keys from disk: %v", err)
	}

	return ds, nil
}

func (d *DiskStore) Get(key string) (string, error) {
	// Get retrieves the value from the disk and returns. If the key does not
	// exist then it returns an empty string
	//
	// How get works?
	//	1. Check if there is any KeyEntry record for the key in keyDir
	//	2. Return an empty string if key doesn't exist or if the key has been deleted
	//	3. If it exists, then read KeyEntry.totalSize bytes starting from the
	//     KeyEntry.position from the disk
	//	4. Decode the bytes into valid KV pair and return the value
	//
	kEntry, ok := d.keyDir[key]
	if !ok {
		return "", ErrKeyNotFound
	}

	// move the current pointer to the right offset
	_, err := kEntry.file.Seek(int64(kEntry.position), defaultWhence)
	if err != nil {
		return "", ErrSeekFailed
	}

	data := make([]byte, kEntry.totalSize)
	_, err = io.ReadFull(kEntry.file, data)
	if err != nil {
		return "", ErrReadFailed
	}

	result := &Record{}
	err = result.DecodeKV(data)
	if err != nil {
		return "", ErrDecodingFailed
	}

	//validate if the checksum matches means the value is not corrupted
	if !result.VerifyCheckSum(data) {
		return "", ErrChecksumMismatch
	}

	return result.Value, nil
}

func (d *DiskStore) Set(key string, value string) error {
	// Set stores the key and value on the disk
	//
	// The steps to save a KV to disk is simple:
	// 1. Encode the KV into bytes
	// 2. Write the bytes to disk by appending to the file
	// 3. Update KeyDir with the KeyEntry of this key

	if err := validateKV(key, []byte(value)); err != nil {
		return err
	}

	timestamp := uint32(time.Now().Unix())
	h := Header{TimeStamp: timestamp, KeySize: uint32(len(key)), ValueSize: uint32(len(value))}
	r := Record{Header: h, Key: key, Value: value, RecordSize: headerSize + h.KeySize + h.ValueSize}
	r.Header.CheckSum = r.CalculateCheckSum()

	//encode the record
	buf := new(bytes.Buffer)
	err := r.EncodeKV(buf)
	if err != nil {
		return ErrEncodingFailed
	}
	d.write(buf.Bytes())

	d.keyDir[key] = NewKeyEntry(timestamp, d.file, uint32(d.writePosition), r.Size())
	// update last write position, so that next record can be written from this point
	d.writePosition += int(r.Size())

	return nil
}

func (d *DiskStore) checkMaxFileSizeReached(size int) error {
	if d.file == nil {
		err := d.createNewDataFile()
		return err
	}

	stat, _ := d.file.Stat()
	nextSize := stat.Size() + int64(size)
	if nextSize > MaxFileSize {
		err := d.createNewDataFile()

		return err
	}

	return nil
}

func (d *DiskStore) createNewDataFile() error {
	activeFile := createFilenameId(d.file.Name()) + ".bitcask.data"
	file, err := os.Create(filepath.Join(d.dir, activeFile))
	if err != nil {
		return err
	}
	d.file = file
	d.writePosition = 0

	return nil
}

func (d *DiskStore) Delete(key string) error {
	timestamp := uint32(time.Now().Unix())
	value := ""
	h := Header{TimeStamp: timestamp, KeySize: uint32(len(key)), ValueSize: uint32(len(value))}

	// mark as tombstone
	h.MarkTombStone()
	r := Record{Header: h, Key: key, Value: value, RecordSize: headerSize + h.KeySize + h.ValueSize}
	r.Header.CheckSum = r.CalculateCheckSum()

	buf := new(bytes.Buffer)
	err := r.EncodeKV(buf)
	if err != nil {
		return err
	}
	d.write(buf.Bytes())

	//delete the key from the hash table
	delete(d.keyDir, key)
	return nil
}

func (d *DiskStore) Close() bool {
	// before we close the file, we need to safely write the contents in the buffers
	// to the disk. Check documentation of DiskStore.write() to understand
	// following the operations
	// TODO: handle errors
	if d.file != nil {
		d.file.Sync()
		if err := d.file.Close(); err != nil {
			// TODO: log the error
			return false
		}
	}
	for _, v := range d.keyDir {
		v.file.Close()
	}
	return true
}

func (d *DiskStore) write(data []byte) {
	if err := d.checkMaxFileSizeReached(len(data)); err != nil {
		panic(err)
	}

	// saving stuff to a file reliably is hard!
	// if you would like to explore and learn more, then
	// start from here: https://danluu.com/file-consistency/
	// and read this too: https://lwn.net/Articles/457667/
	if _, err := d.file.Write(data); err != nil {
		panic(err)
	}
	// calling fsync after every write is important, this assures that our writes
	// are actually persisted to the disk
	if err := d.file.Sync(); err != nil {
		panic(err)
	}
}

func (d *DiskStore) initKeyDir(directoryName string) error {
	dirEntries, err := os.ReadDir(directoryName)
	if err != nil {
		return err
	}

	for _, entry := range dirEntries {
		if entry.IsDir() {
			continue
		}

		err = initKeyDirInternal(d.keyDir, filepath.Join(d.dir, entry.Name()))
		if err != nil {
			return err
		}
	}

	fileName := createFilenameId("") + ".bitcask.data"

	if len(dirEntries) > 0 {
		fileName = createFilenameId(dirEntries[len(dirEntries)-1].Name()) + ".bitcask.data"
	}

	file, err := os.Create(filepath.Join(d.dir, fileName))
	if err != nil {
		return err
	}
	d.file = file
	d.writePosition = 0

	return nil
}

func createFilenameId(filename string) string {
	if filename == "" {
		return "1000000000"
	}
	pattern := regexp.MustCompile(`(\d+)\.bitcask`)
	matches := pattern.FindStringSubmatch(filename)

	filenameId, _ := strconv.Atoi(matches[1])

	return strconv.Itoa(filenameId + 1)
}

func initKeyDirInternal(keyDir map[string]KeyEntry, existingFile string) error {
	// we will initialise the keyDir by reading the contents of the file, record by
	// record. As we read each record, we will also update our keyDir with the
	// corresponding KeyEntry
	//
	// NOTE: this method is a blocking one, if the DB size is yuge then it will take
	// a lot of time to startup
	file, _ := os.Open(existingFile)
	writePosition := 0

	for {
		header := make([]byte, headerSize)
		_, err := io.ReadFull(file, header)

		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		h, err := NewHeader(header)
		if err != nil {
			return err
		}

		key := make([]byte, h.KeySize)
		value := make([]byte, h.ValueSize)

		_, err = io.ReadFull(file, key)
		if err != nil {
			return err
		}

		_, err = io.ReadFull(file, value)
		if err != nil {
			return err
		}

		totalSize := headerSize + h.KeySize + h.ValueSize
		keyDir[string(key)] = NewKeyEntry(h.TimeStamp, file, uint32(writePosition), totalSize)
		writePosition += int(totalSize)
	}
	return nil
}

// returns a list of the current keys
func (d *DiskStore) ListKeys() []string {
	result := make([]string, 0, len(d.keyDir))

	for k := range d.keyDir {
		result = append(result, k)
	}

	return result
}
