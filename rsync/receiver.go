package rsync

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"sort"
	"time"

	"github.com/kaiakz/ubuffer"
)

/* Receiver:
1. Receive File list
2. Request files by sending files' index
3. Receive Files, pass the files to Storage
*/
type Receiver struct {
	Conn    *Conn
	Module  string
	Path    string
	Seed    int32
	LVer    int32
	RVer    int32
	Storage FS
}

func (r *Receiver) BuildArgs() string {
	return ""
}

// DeMux was started here
func (r *Receiver) StartMuxIn() {
	r.Conn.Reader = NewMuxReaderV0(r.Conn.Reader)
}

func (r *Receiver) SendExclusions() error {
	// Send exclusion
	return r.Conn.WriteInt(EXCLUSION_END)
}

// Return a filelist from remote
func (r *Receiver) RecvFileList() (FileList, map[string][]byte, error) {
	filelist := make(FileList, 0, 1*M)
	symlinks := make(map[string][]byte)
	for {
		flags, err := r.Conn.ReadByte()
		if err != nil {
			return filelist, symlinks, err
		}

		if flags == FLIST_END {
			break
		}
		//fmt.Printf("[%d]\n", flags)

		lastIndex := len(filelist) - 1
		var partial, pathlen uint32 = 0, 0

		/*
		 * Read our filename.
		 * If we have FLIST_NAME_SAME, we inherit some of the last
		 * transmitted name.
		 * If we have FLIST_NAME_LONG, then the string length is greater
		 * than byte-size.
		 */
		if (flags & FLIST_NAME_SAME) != 0 {
			val, err := r.Conn.ReadByte()
			if err != nil {
				return filelist, symlinks, err
			}
			partial = uint32(val)
			//fmt.Println("Partical", partial)
		}

		/* Get the (possibly-remaining) filename length. */
		if (flags & FLIST_NAME_LONG) != 0 {
			val, err := r.Conn.ReadInt()
			if err != nil {
				return filelist, symlinks, err
			}
			pathlen = uint32(val) // can't use for rsync 31

		} else {
			val, err := r.Conn.ReadByte()
			if err != nil {
				return filelist, symlinks, err
			}
			pathlen = uint32(val)
		}
		//fmt.Println("PathLen", pathlen)

		/* Allocate our full filename length. */
		/* FIXME: maximum pathname length. */
		// TODO: if pathlen + partical == 0
		// malloc len error?

		p := make([]byte, pathlen)
		_, err = io.ReadFull(r.Conn, p)
		if err != nil {
			return filelist, symlinks, err
		}

		path := make([]byte, 0, partial+pathlen)
		/* If so, use last */
		if (flags & FLIST_NAME_SAME) != 0 { // FLIST_NAME_SAME
			last := filelist[lastIndex]
			path = append(path, last.Path[0:partial]...)
		}
		path = append(path, p...)
		//fmt.Println("Path ", string(path))

		size, err := r.Conn.ReadVarint()
		if err != nil {
			return filelist, symlinks, err
		}
		//fmt.Println("Size ", size)

		/* Read the modification time. */
		var mtime int32
		if (flags & FLIST_TIME_SAME) == 0 {
			mtime, err = r.Conn.ReadInt()
			if err != nil {
				return filelist, symlinks, err
			}
		} else {
			mtime = filelist[lastIndex].Mtime
		}
		//fmt.Println("MTIME ", mtime)

		/* Read the file mode. */
		var mode FileMode
		if (flags & FLIST_MODE_SAME) == 0 {
			val, err := r.Conn.ReadInt()
			if err != nil {
				return filelist, symlinks, err
			}
			mode = FileMode(val)
		} else {
			mode = filelist[lastIndex].Mode
		}
		//fmt.Println("Mode", uint32(mode))

		// TODO: Sym link
		if ((mode & 32768) != 0) && ((mode & 8192) != 0) {
			sllen, err := r.Conn.ReadInt()
			if err != nil {
				return filelist, symlinks, err
			}
			slink := make([]byte, sllen)
			_, err = io.ReadFull(r.Conn, slink)
			symlinks[string(path)] = slink
			if err != nil {
				return filelist, symlinks, errors.New("failed to read symlink")
			}
			//fmt.Println("Symbolic Len:", len, "Content:", slink)
		}

		fmt.Println("@", string(path), mode, size, mtime)

		filelist = append(filelist, FileInfo{
			Path:  path,
			Size:  size,
			Mtime: mtime,
			Mode:  mode,
		})
	}

	// Sort the filelist lexicographically
	sort.Sort(filelist)

	return filelist, symlinks, nil
}

// Generator: handle files: if it's a regular file, send its index. Otherwise, put them to Storage
func (r *Receiver) Generator(remoteList FileList, downloadList []int, symlinks map[string][]byte) error {
	emptyBlocks := make([]byte, 16) // 4 + 4 + 4 + 4 bytes, all bytes set to 0
	content := new(bytes.Buffer)

	for _, v := range downloadList {
		if remoteList[v].Mode.IsREG() {
			if err := r.Conn.WriteInt(int32(v)); err != nil {
				log.Println("Failed to send index")
				return err
			}
			//fmt.Println("Request: ", string(remoteList[v].Path), uint32(remoteList[v].Mode))
			if _, err := r.Conn.Write(emptyBlocks); err != nil {
				return err
			}
		} else {
			// TODO: Supports more file mode
			// EXPERIMENTAL
			// Handle folders & symbol links
			content.Reset()
			size := remoteList[v].Size
			if remoteList[v].Mode.IsLNK() {
				if _, err := content.Write(symlinks[string(remoteList[v].Path)]); err != nil {
					return err
				}
				size = int64(content.Len())
			}

			if _, err := r.Storage.Put(string(remoteList[v].Path), content, size, FileMetadata{
				Mtime: remoteList[v].Mtime,
				Mode:  remoteList[v].Mode,
			}); err != nil {
				return err
			}
		}
	}

	// Send -1 to finish, then start to download
	if err := r.Conn.WriteInt(INDEX_END); err != nil {
		log.Println("Can't send INDEX_END")
		return err
	}
	log.Println("Request completed")

	startTime := time.Now()
	err := r.FileDownloader(remoteList[:])
	log.Println("Downloaded duration:", time.Since(startTime))
	return err
}

// TODO: It is better to update files in goroutine
func (r *Receiver) FileDownloader(localList FileList) error {

	rmd4 := make([]byte, 16)

	for {
		index, err := r.Conn.ReadInt()
		if err != nil {
			return err
		}
		if index == INDEX_END { // -1 means the end of transfer files
			return nil
		}
		//fmt.Println("INDEX:", index)

		count, err := r.Conn.ReadInt() /* block count */
		if err != nil {
			return err
		}

		blen, err := r.Conn.ReadInt() /* block length */
		if err != nil {
			return err
		}

		clen, err := r.Conn.ReadInt() /* checksum length */
		if err != nil {
			return err
		}

		remainder, err := r.Conn.ReadInt() /* block remainder */
		if err != nil {
			return err
		}

		path := localList[index].Path
		log.Println("Downloading:", string(path), count, blen, clen, remainder, localList[index].Size)

		// If the file is too big to store in memory, creates a temporary file in the directory 'tmp'
		buffer := ubuffer.NewBuffer(localList[index].Size)
		downloadeSize := 0
		bufwriter := bufio.NewWriter(buffer)

		// Create MD4
		//lmd4 := md4.New()
		//if err := binary.Write(lmd4, binary.LittleEndian, r.seed); err != nil {
		//	log.Println("Failed to compute md4")
		//}

		for {
			token, err := r.Conn.ReadInt()
			if err != nil {
				return err
			}
			log.Println("TOKEN", token)
			if token == 0 {
				break
			} else if token < 0 {
				return errors.New("does not support block checksum")
				// Reference
			} else {
				ctx := make([]byte, token) // FIXME: memory leak?
				_, err = io.ReadFull(r.Conn, ctx)
				if err != nil {
					return err
				}
				downloadeSize += int(token)
				log.Println("Downloaded:", downloadeSize, "byte")
				if _, err := bufwriter.Write(ctx); err != nil {
					return err
				}
				//if _, err := lmd4.Write(ctx); err != nil {
				//	return err
				//}
			}
		}
		if bufwriter.Flush() != nil {
			return errors.New("failed to flush buffer")
		}

		// Remote MD4
		// TODO: compare computed MD4 with remote MD4
		_, err = io.ReadFull(r.Conn, rmd4)
		if err != nil {
			return err
		}
		// Compare two MD4
		//if bytes.Compare(rmd4, lmd4.Sum(nil)) != 0 {
		//	log.Println("Checksum error")
		//}

		// Put file to object Storage
		_, err = buffer.Seek(0, io.SeekStart)
		if err != nil {
			return err
		}

		n, err := r.Storage.Put(string(path), buffer, int64(downloadeSize), FileMetadata{
			Mtime: localList[index].Mtime,
			Mode:  localList[index].Mode,
		})
		if err != nil {
			return err
		}

		if buffer.Finalize() != nil {
			return errors.New("buffer can't be finalized")
		}

		log.Printf("Successfully uploaded %s of size %d\n", path, n)
	}
}

// Clean up local files
func (r *Receiver) FileCleaner(localList FileList, deleteList []int) error {
	// Since file list was already sorted, we can iterate it in the reverse direction to traverse the file tree in post-order
	// Thus it always cleans sub-files firstly
	for i := len(deleteList) - 1; i >= 0; i-- {
		fname := string(localList[deleteList[i]].Path)
		err := r.Storage.Delete(fname, localList[deleteList[i]].Mode)
		log.Println("Deleted:", fname)
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *Receiver) FinalPhase() error {
	//go func() {
	//	ioerror, err := r.Conn.ReadInt()
	//	log.Println(ioerror, err)
	//}()

	err := r.Conn.WriteInt(INDEX_END)
	if err != nil {
		return err
	}
	return r.Conn.WriteInt(INDEX_END)
}

func (r *Receiver) Sync() error {
	defer func() {
		log.Println("Task completed", r.Conn.Close()) // TODO: How to handle errors from Close
	}()

	lfiles, err := r.Storage.List()
	if err != nil {
		return err
	}
	//for _, v := range lfiles {
	//	fmt.Println("Local File:", string(v.Path), v.Mode, v.Mtime)
	//}

	rfiles, symlinks, err := r.RecvFileList()
	if err != nil {
		return err
	}
	log.Println("Remote files count:", len(rfiles))

	ioerr, err := r.Conn.ReadInt()
	if err != nil {
		return nil
	}
	log.Println("IOERR", ioerr)

	newfiles, oldfiles := lfiles.Diff(rfiles)
	if len(newfiles) == 0 && len(oldfiles) == 0 {
		log.Println("There is nothing to do")
	}
	fmt.Print(newfiles, oldfiles)

	if err := r.Generator(rfiles[:], newfiles[:], symlinks); err != nil {
		return err
	}
	if err := r.FileCleaner(lfiles[:], oldfiles[:]); err != nil {
		return err
	}
	if err := r.FinalPhase(); err != nil {
		return err
	}
	return nil
}
