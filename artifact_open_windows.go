package bridge

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

func openArtifact(path string, relative []string) (*os.File, error) {
	absolute := filepath.Clean(path)
	volume := filepath.VolumeName(absolute)
	if !filepath.IsAbs(absolute) || len(volume) != 2 || volume[1] != ':' {
		return nil, errUnsafeArtifact
	}
	drive := volume + "\\"
	driveName, err := windows.UTF16PtrFromString(drive)
	if err != nil || windows.GetDriveType(driveName) != windows.DRIVE_FIXED {
		return nil, errUnsafeArtifact
	}
	handle, err := windows.CreateFile(driveName, windows.FILE_GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	defer func() { windows.CloseHandle(handle) }()
	segments := append(strings.Split(strings.TrimPrefix(absolute, drive), "\\"), relative...)
	for index, segment := range segments {
		if !filepath.IsLocal(segment) || strings.ContainsAny(segment, ":/\\") || segment == "." || strings.HasSuffix(segment, ".") || strings.HasSuffix(segment, " ") {
			return nil, errUnsafeArtifact
		}
		name, err := windows.NewNTUnicodeString(segment)
		if err != nil {
			return nil, err
		}
		attributes := windows.OBJECT_ATTRIBUTES{RootDirectory: handle, ObjectName: name, Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE}
		attributes.Length = uint32(unsafe.Sizeof(attributes))
		last := index == len(segments)-1
		options := uint32(windows.FILE_OPEN_REPARSE_POINT | windows.FILE_SYNCHRONOUS_IO_NONALERT | windows.FILE_DIRECTORY_FILE)
		share := uint32(windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE)
		if last {
			options = windows.FILE_OPEN_REPARSE_POINT | windows.FILE_SYNCHRONOUS_IO_NONALERT | windows.FILE_NON_DIRECTORY_FILE
			share = windows.FILE_SHARE_READ
		}
		var child windows.Handle
		var status windows.IO_STATUS_BLOCK
		if err := windows.NtCreateFile(&child, windows.FILE_GENERIC_READ, &attributes, &status, nil, windows.FILE_ATTRIBUTE_NORMAL,
			share, windows.FILE_OPEN, options, 0, 0); err != nil {
			return nil, err
		}
		var info windows.ByHandleFileInformation
		if err := windows.GetFileInformationByHandle(child, &info); err != nil ||
			info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || last && info.NumberOfLinks != 1 {
			windows.CloseHandle(child)
			return nil, errUnsafeArtifact
		}
		if last {
			if kind, err := windows.GetFileType(child); err != nil || kind != windows.FILE_TYPE_DISK {
				windows.CloseHandle(child)
				return nil, errUnsafeArtifact
			}
			file := os.NewFile(uintptr(child), segment)
			if file == nil {
				windows.CloseHandle(child)
				return nil, errors.New("could not wrap artifact file handle")
			}
			return file, nil
		}
		windows.CloseHandle(handle)
		handle = child
	}
	return nil, errUnsafeArtifact
}
