package postgres

import (
	"archive/tar"
	"errors"
	"os"

	"github.com/wal-g/tracelog"

	"github.com/jackc/pgx"
	"github.com/wal-g/wal-g/internal"
	"github.com/wal-g/wal-g/pkg/storages/storage"
)

// TarBallComposer is used to compose files into tarballs.
type TarBallComposer interface {
	AddFile(info *ComposeFileInfo)
	AddHeader(header *tar.Header, fileInfo os.FileInfo) error
	SkipFile(tarHeader *tar.Header, fileInfo os.FileInfo)
	PackTarballs() (TarFileSets, error)
	GetFiles() BundleFiles
}

// ComposeFileInfo holds data which is required to pack a file to some tarball
type ComposeFileInfo struct {
	path          string
	fileInfo      os.FileInfo
	wasInBase     bool
	header        *tar.Header
	isIncremented bool
}

type TarFileSets map[string][]string

func NewComposeFileInfo(path string, fileInfo os.FileInfo, wasInBase, isIncremented bool,
	header *tar.Header) *ComposeFileInfo {
	return &ComposeFileInfo{path: path, fileInfo: fileInfo,
		wasInBase: wasInBase, header: header, isIncremented: isIncremented}
}

type TarBallComposerType int

const (
	RegularComposer TarBallComposerType = iota + 1
	RatingComposer
	CopyComposer
)

// TarBallComposerMaker is used to make an instance of TarBallComposer
type TarBallComposerMaker interface {
	Make(bundle *Bundle) (TarBallComposer, error)
}

func NewTarBallComposerMaker(composerType TarBallComposerType, conn *pgx.Conn,
	folder storage.Folder, newBackupName string,
	filePackOptions TarBallFilePackerOptions) (TarBallComposerMaker, error) {
	switch composerType {
	case RegularComposer:
		return NewRegularTarBallComposerMaker(filePackOptions), nil
	case RatingComposer:
		relFileStats, err := newRelFileStatistics(conn)
		if err != nil {
			return nil, err
		}
		return NewRatingTarBallComposerMaker(relFileStats, filePackOptions)
	case CopyComposer:
		previousBackupName, err := internal.GetLatestBackupName(folder)
		if err != nil {
			tracelog.InfoLogger.Printf(
				"Failed to init the CopyComposer, will use the RegularComposer instead:"+
					" couldn't get the previous backup name: %v", err)
			return NewRegularTarBallComposerMaker(filePackOptions), nil
		}
		previousBackup := NewBackup(folder, previousBackupName)
		prevBackupSentinelDto, err := previousBackup.GetSentinel()
		if err != nil {
			return nil, err
		}
		if prevBackupSentinelDto.IncrementFullName != nil {
			previousBackupName = *prevBackupSentinelDto.IncrementFullName
			previousBackup = NewBackup(folder, previousBackupName)
			_, err = previousBackup.GetSentinel()
			if err != nil {
				return nil, err
			}
		}
		return NewCopyTarBallComposerMaker(previousBackup, newBackupName, filePackOptions), nil
	default:
		return nil, errors.New("NewTarBallComposerMaker: Unknown TarBallComposerType")
	}
}
