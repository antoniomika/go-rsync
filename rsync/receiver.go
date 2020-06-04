package rsync

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
)

// Header: '@RSYNCD: 31.0\n' + ? + '\n' + arguments + '\0'
// Header len 8		AUTHREQD: 18	"@RSYNCD: EXIT" 13		RSYNC_MODULE_LIST_QUERY "\n"


// See clienserver.c start_inband_exchange
func HandShake(conn net.Conn) {
	// send my version
	// send("@RSYNCD: 31.0\n");
	conn.Write([]byte("@RSYNCD: 27.0\n"))

	// receive server's protocol version and seed
	version_str, _ := ReadLine(conn)

	// recv(version)
	var remote_protocol, remote_sub int
	fmt.Sscanf(version_str, "@RSYNCD: %d.%d", remote_protocol, remote_sub)
	log.Println(version_str)

	// send mod name
	// send("Foo\n")
	conn.Write([]byte("epel\n"))
	// conn.Write([]byte("\n"))

	for {
		// Wait for '@RSYNCD: OK': until \n, then add \0
		res, _ := ReadLine(conn)
		log.Print(res)
		if strings.HasPrefix(res, "@RSYNCD: OK") {
			break
		}
	}

	// send parameters list
	//conn.Write([]byte("--server\n--sender\n-g\n-l\n-o\n-p\n-D\n-r\n-t\n.\nepel/7/SRPMS\n\n"))
	conn.Write([]byte("--server\n--sender\n-l\n-p\n-r\n-t\n.\nepel/7/SRPMS\n\n"))	// without gid, uid, mdev

	// read int32 as seed
	bseed := ReadInteger(conn)
	log.Println("SEED", bseed)

	// send filter_list, empty is 32-bit zero
	conn.Write([]byte("\x00\x00\x00\x00"))
}

type FileInfo struct {
	Path string
	Size int64
	Mtime int32
	Mode int32
}

type FileList []FileInfo

func (I FileList) Len() int {
	return len(I)
}

func (I FileList) Less(i, j int) bool {
	if strings.Compare(I[i].Path, I[j].Path) == -1 {
		return true
	}
	return false
}

func (I FileList) Swap(i, j int) {
	I[i], I[j] = I[j], I[i]
}

// file list: ends with '\0'
func GetFileList(ds chan byte, filelist *FileList) error {

	flags := <- ds

	var partial, pathlen uint32 = 0, 0

	log.Println(">>#{flags}<<")

	if flags == 0 {
		return io.EOF
	}

	/*
	 * Read our filename.
	 * If we have FLIST_NAME_SAME, we inherit some of the last
	 * transmitted name.
	 * If we have FLIST_NAME_LONG, then the string length is greater
	 * than byte-size.
	 */
	if (0x20 & flags) != 0 {
		partial = uint32(GetByte(ds))
		log.Println("Partical", partial)
	}

	/* Get the (possibly-remaining) filename length. */
	if (0x40 & flags) != 0 {
		pathlen = uint32(GetInteger(ds)) // can't use for rsync 31

	} else {
		pathlen = uint32(<-ds)
	}
	log.Println("PathLen", pathlen)

	/* Allocate our full filename length. */
	/* FIXME: maximum pathname length. */
	// TODO: if pathlen + partical == 0
	// malloc len error?

	// last := (*filelist)[len(*filelist) - 1]	// FIXME


	p := make([]byte, pathlen)
	GetBytes(ds, p)
	var path string
	/* If so, use last */
	if (0x20 & flags) != 0 {	// FLIST_NAME_SAME
		last := (*filelist)[len(*filelist) - 1]
		path = last.Path[0: partial]
	}
	path += string(p)
	log.Println("Path ", path)

	size := GetVarint(ds)
	log.Println("Size ", size)

	/* Read the modification time. */
	var mtime int32
	if (flags & 0x80) == 0 {
		mtime = GetInteger(ds)

	} else {
		mtime = (*filelist)[len(*filelist) - 1].Mtime
	}
	log.Println("MTIME ", mtime)

	/* Read the file mode. */
	var mode int32
	if (flags & 0x02) == 0 {
		mode = GetInteger(ds)

	} else {
		mode = (*filelist)[len(*filelist) - 1].Mode
	}
	log.Println("Mode", uint32(mode))

	// FIXME: Sym link
	if ((mode & 32768) != 0) && ((mode & 8192) != 0) {
		len := uint32(GetInteger(ds))
		slink := make([]byte, len)
		GetBytes(ds, slink)
		log.Println("Symbolic Len", len, "CTX", slink)
	}

	*filelist = append(*filelist, FileInfo{
		Path:  path,
		Size:  size,
		Mtime: mtime,
		Mode:  mode,
	})

	return nil
}


func Generate(conn net.Conn, filelist *FileList) {
	// Compare all local files with file list, pick up the files that has different size, mtime
	// Those files are `basis files`
	var idx int32

	// TODO: Supports multi files
	// For test: here we request a file
	for i:=0; i < len(*filelist); i++ {
		if strings.Index((*filelist)[i].Path, "0ad-data-0.0.22-1.el7.src.rpm") != -1 {	// 95533 SRPMS/Packages/z/zanata-python-client-1.5.1-1.el7.src.rpmSRPMS/Packages/0/0ad-0.0.22-1.el7.src.rpm
			idx = int32(i)
			log.Println("Pick:", (*filelist)[i], idx)
			break
		}
	}

	// identifier
	binary.Write(conn, binary.LittleEndian, idx)



	// block count, block length(default is 32768?), checksum length(default is 2?), block remainder, blocks(short+long)
	// Just let them be empty(zero)
	empty := make([]byte, 16)	// 4 + 4 + 4 + 4 bytes
	conn.Write(empty)	// ENDIAN?

	//conn.Write(empty)
	//binary.Write(conn, binary.LittleEndian, int32(0))	// 32768
	//binary.Write(conn, binary.LittleEndian, int32(0))	// 2
	//conn.Write(empty)

	// Empty checksum

	// Finish
	binary.Write(conn, binary.LittleEndian, int32(-1))

}

// a block: [file id + block checksum + '\0']

func exchangeBlock() {
// Here we get a list stores old files
// Rolling Checksum & Hash value
// Loop until all file are updated, each time handle a file.
	// Send a empty signature block (no Rolling Checksum & Hash value)
	// Download the data blocks, and write them into a file
}