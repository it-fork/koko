package httpd

import (
	"cocogo/pkg/common"
	"cocogo/pkg/srvconn"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/LeeEirc/elfinder"
	"github.com/pkg/sftp"
	gossh "golang.org/x/crypto/ssh"

	"cocogo/pkg/config"
	"cocogo/pkg/logger"
	"cocogo/pkg/model"
	"cocogo/pkg/service"
)

var (
	defaultHomeName = "Home"
)

func NewUserVolume(user *model.User, addr string) *UserVolume {
	rawID := fmt.Sprintf("'%s@%s", user.Username, addr)
	fmt.Println(rawID)
	uVolume := &UserVolume{
		Uuid:     elfinder.GenerateID(rawID),
		user:     user,
		Name:     defaultHomeName,
		basePath: fmt.Sprintf("/%s", defaultHomeName),
	}
	uVolume.initial()
	return uVolume
}

type UserVolume struct {
	Uuid     string
	Name     string
	basePath string
	user     *model.User
	assets   model.AssetList

	rootPath string //  tmp || home || ~
	hosts    map[string]*hostnameVolume

	localTmpPath string
}

func (u *UserVolume) initial() {
	conf := config.GetConf()
	u.loadAssets()
	u.rootPath = conf.SftpRoot
	u.localTmpPath = filepath.Join(conf.RootPath, "data", "tmp")
	_ = common.EnsureDirExist(u.localTmpPath)
	u.hosts = make(map[string]*hostnameVolume)
	for i, item := range u.assets {
		tmpDir := &hostnameVolume{
			VID:      u.ID(),
			homePath: u.basePath,
			hostPath: filepath.Join(u.basePath, item.Hostname),
			asset:    &u.assets[i],
			time:     time.Now().UTC(),
		}
		u.hosts[item.Hostname] = tmpDir
	}
}

func (u *UserVolume) loadAssets() {
	u.assets = service.GetUserAssets(u.user.ID, "1")
}

func (u *UserVolume) ID() string {
	return u.Uuid

}

func (u *UserVolume) Info(path string) (elfinder.FileDir, error) {
	var rest elfinder.FileDir
	if path == "" || path == "/" {
		path = u.basePath
	}
	if path == u.basePath {
		return u.RootFileDir(), nil
	}
	pathNames := strings.Split(strings.TrimPrefix(path, "/"), "/")
	hostDir, ok := u.hosts[pathNames[1]]
	if !ok {
		return rest, os.ErrNotExist
	}
	if hostDir.hostPath == path {
		return hostDir.info(), nil
	}

	if hostDir.suMaps == nil {
		hostDir.suMaps = make(map[string]*sysUserVolume)
		systemUsers := hostDir.asset.SystemUsers
		for i, sysUser := range systemUsers {
			hostDir.suMaps[sysUser.Name] = &sysUserVolume{
				VID:        u.ID(),
				hostpath:   hostDir.hostPath,
				suPath:     filepath.Join(hostDir.hostPath, sysUser.Name),
				systemUser: &systemUsers[i],
				rootPath:   u.rootPath,
			}
		}
	}

	sysUserDir, ok := hostDir.suMaps[pathNames[2]]
	if !ok {
		return rest, os.ErrNotExist
	}
	if path == sysUserDir.suPath {
		return sysUserDir.info(), nil
	}

	realPath := sysUserDir.ParsePath(path)
	if sysUserDir.client == nil {
		sftClient, conn, err := u.GetSftpClient(hostDir.asset, sysUserDir.systemUser)
		if err != nil {
			return rest, os.ErrPermission
		}
		sysUserDir.client = sftClient
		sysUserDir.conn = conn

	}
	dirname := filepath.Dir(path)
	fileInfos, err := sysUserDir.client.Stat(realPath)
	if err != nil {
		return rest, err
	}
	rest.Name = fileInfos.Name()
	rest.Hash = hashPath(u.ID(), path)
	rest.Phash = hashPath(u.ID(), dirname)
	rest.Size = fileInfos.Size()
	rest.Volumeid = u.ID()
	if fileInfos.IsDir() {
		rest.Mime = "directory"
		rest.Dirs = 1
	} else {
		rest.Mime = "file"
		rest.Dirs = 0
	}
	rest.Read, rest.Write = elfinder.ReadWritePem(fileInfos.Mode())
	return rest, nil
}

func (u *UserVolume) List(path string) []elfinder.FileDir {
	var dirs []elfinder.FileDir
	if path == "" || path == "/" {
		path = u.basePath
	}
	if path == u.basePath {
		dirs = make([]elfinder.FileDir, 0, len(u.hosts))
		for _, item := range u.hosts {
			dirs = append(dirs, item.info())
		}
		return dirs
	}
	pathNames := strings.Split(strings.TrimPrefix(path, "/"), "/")
	hostDir, ok := u.hosts[pathNames[1]]
	if !ok {
		return dirs
	}

	if hostDir.suMaps == nil {
		hostDir.suMaps = make(map[string]*sysUserVolume)
		systemUsers := hostDir.asset.SystemUsers
		for i, sysUser := range systemUsers {
			hostDir.suMaps[sysUser.Name] = &sysUserVolume{
				VID:        u.ID(),
				hostpath:   hostDir.hostPath,
				suPath:     filepath.Join(hostDir.hostPath, sysUser.Name),
				systemUser: &systemUsers[i],
				rootPath:   u.rootPath,
			}
		}
	}
	if hostDir.hostPath == path {
		dirs = make([]elfinder.FileDir, 0, len(hostDir.suMaps))
		for _, item := range hostDir.suMaps {
			dirs = append(dirs, item.info())
		}
		return dirs
	}
	sysUserDir, ok := hostDir.suMaps[pathNames[2]]
	if !ok {
		return dirs
	}
	realPath := sysUserDir.ParsePath(path)

	if sysUserDir.client == nil {
		sftClient, conn, err := u.GetSftpClient(hostDir.asset, sysUserDir.systemUser)
		if err != nil {
			return dirs
		}
		sysUserDir.client = sftClient
		sysUserDir.conn = conn
	}
	subFiles, err := sysUserDir.client.ReadDir(realPath)
	if err != nil {
		return dirs
	}
	dirs = make([]elfinder.FileDir, 0, len(subFiles))
	for _, fInfo := range subFiles {
		fileDir, err := u.Info(filepath.Join(path, fInfo.Name()))
		if err != nil {
			continue
		}
		dirs = append(dirs, fileDir)
	}
	return dirs
}

func (u *UserVolume) Parents(path string, dep int) []elfinder.FileDir {
	relativepath := strings.TrimPrefix(strings.TrimPrefix(path, u.basePath), "/")
	relativePaths := strings.Split(relativepath, "/")
	dirs := make([]elfinder.FileDir, 0, len(relativePaths))

	for i := range relativePaths {
		realDirPath := filepath.Join(u.basePath, filepath.Join(relativePaths[:i]...))
		result, err := u.Info(realDirPath)
		if err != nil {
			continue
		}
		dirs = append(dirs, result)
		tmpDir := u.List(realDirPath)
		for j, item := range tmpDir {
			if item.Dirs == 1 {
				dirs = append(dirs, tmpDir[j])
			}
		}
	}
	return dirs
}

func (u *UserVolume) GetFile(path string) (reader io.ReadCloser, err error) {
	pathNames := strings.Split(strings.TrimPrefix(path, "/"), "/")
	hostDir, ok := u.hosts[pathNames[1]]
	if !ok {
		return nil, os.ErrNotExist
	}
	if hostDir.suMaps == nil {
		hostDir.suMaps = make(map[string]*sysUserVolume)
		systemUsers := hostDir.asset.SystemUsers
		for i, sysUser := range systemUsers {
			hostDir.suMaps[sysUser.Name] = &sysUserVolume{
				VID:        u.ID(),
				hostpath:   hostDir.hostPath,
				suPath:     filepath.Join(hostDir.hostPath, sysUser.Name),
				systemUser: &systemUsers[i],
				rootPath:   u.rootPath,
			}
		}
	}
	sysUserDir, ok := hostDir.suMaps[pathNames[2]]
	if !ok {
		return nil, os.ErrNotExist
	}
	realPath := sysUserDir.ParsePath(path)
	if sysUserDir.client == nil {
		sftClient, conn, err := u.GetSftpClient(hostDir.asset, sysUserDir.systemUser)
		if err != nil {
			return nil, os.ErrPermission
		}
		sysUserDir.client = sftClient
		sysUserDir.conn = conn

	}
	return sysUserDir.client.Open(realPath)
}

func (u *UserVolume) UploadFile(dir, filename string, reader io.Reader) (elfinder.FileDir, error) {
	var rest elfinder.FileDir
	var err error
	if dir == "" || dir == "/" {
		dir = u.basePath
	}
	if dir == u.basePath {
		return rest, os.ErrPermission
	}
	pathNames := strings.Split(strings.TrimPrefix(dir, "/"), "/")
	hostDir, ok := u.hosts[pathNames[1]]
	if !ok {
		return rest, os.ErrNotExist
	}
	if hostDir.hostPath == dir {
		return rest, os.ErrPermission
	}
	if hostDir.suMaps == nil {
		hostDir.suMaps = make(map[string]*sysUserVolume)
		systemUsers := hostDir.asset.SystemUsers
		for i, sysUser := range systemUsers {
			hostDir.suMaps[sysUser.Name] = &sysUserVolume{
				VID:        u.ID(),
				hostpath:   hostDir.hostPath,
				suPath:     filepath.Join(hostDir.hostPath, sysUser.Name),
				systemUser: &systemUsers[i],
				rootPath:   u.rootPath,
			}
		}
	}

	sysUserDir, ok := hostDir.suMaps[pathNames[2]]
	if !ok {
		return rest, os.ErrNotExist
	}
	realPath := sysUserDir.ParsePath(dir)
	if sysUserDir.client == nil {
		sftClient, conn, err := u.GetSftpClient(hostDir.asset, sysUserDir.systemUser)
		if err != nil {
			return rest, os.ErrPermission
		}
		sysUserDir.client = sftClient
		sysUserDir.conn = conn

	}
	realFilenamePath := filepath.Join(realPath, filename)

	fd, err := sysUserDir.client.Create(realFilenamePath)
	if err != nil {
		return rest, err
	}
	defer fd.Close()
	_, err = io.Copy(fd, reader)
	if err != nil {
		return rest, err
	}
	return u.Info(filepath.Join(dir, filename))
}

func (u *UserVolume) UploadChunk(cid int, dirPath, chunkName string, reader io.Reader) error {
	//chunkName format "filename.[NUMBER]_[TOTAL].part"
	var err error
	tmpDir := filepath.Join(u.localTmpPath, dirPath)
	err = common.EnsureDirExist(tmpDir)
	if err != nil {
		return err
	}
	chunkRealPath := fmt.Sprintf("%s_%d",
		filepath.Join(tmpDir, chunkName), cid)

	fd, err := os.Create(chunkRealPath)
	defer fd.Close()
	if err != nil {
		return err
	}
	_, err = io.Copy(fd, reader)
	fmt.Println(err)
	return err
}

func (u *UserVolume) MergeChunk(cid, total int, dirPath, filename string) (elfinder.FileDir, error) {
	var rest elfinder.FileDir
	if u.basePath == dirPath {
		return rest, os.ErrPermission
	}
	pathNames := strings.Split(strings.TrimPrefix(dirPath, "/"), "/")
	hostDir, ok := u.hosts[pathNames[1]]
	if !ok {
		return rest, os.ErrNotExist
	}
	if hostDir.hostPath == dirPath {
		return rest, os.ErrPermission
	}
	if hostDir.suMaps == nil {
		hostDir.suMaps = make(map[string]*sysUserVolume)
		systemUsers := hostDir.asset.SystemUsers
		for i, sysUser := range systemUsers {
			hostDir.suMaps[sysUser.Name] = &sysUserVolume{
				VID:        u.ID(),
				hostpath:   hostDir.hostPath,
				suPath:     filepath.Join(hostDir.hostPath, sysUser.Name),
				systemUser: &systemUsers[i],
				rootPath:   u.rootPath,
			}
		}
	}

	sysUserDir, ok := hostDir.suMaps[pathNames[2]]
	if !ok {
		return rest, os.ErrNotExist
	}
	realDirPath := sysUserDir.ParsePath(dirPath)
	if sysUserDir.client == nil {
		sftClient, conn, err := u.GetSftpClient(hostDir.asset, sysUserDir.systemUser)
		if err != nil {
			return rest, os.ErrPermission
		}
		sysUserDir.client = sftClient
		sysUserDir.conn = conn

	}
	filenamePath := filepath.Join(realDirPath, filename)
	fd, err := sysUserDir.client.OpenFile(filenamePath, os.O_WRONLY|os.O_APPEND|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return rest, err
	}
	defer fd.Close()
	for i := 0; i <= total; i++ {
		partPath := fmt.Sprintf("%s.%d_%d.part_%d",
			filepath.Join(u.localTmpPath, dirPath, filename), i, total, cid)

		partFD, err := os.Open(partPath)
		if err != nil {
			return rest, err
		}
		_, err = io.Copy(fd, partFD)
		if err != nil {
			return rest, os.ErrNotExist
		}
		_ = partFD.Close()
		err = os.Remove(partPath)
	}

	return u.Info(filepath.Join(dirPath, filename))
}

func (u *UserVolume) CompleteChunk(cid, total int, dirPath, filename string) bool {
	for i := 0; i <= total; i++ {
		partPath := fmt.Sprintf("%s.%d_%d.part_%d",
			filepath.Join(u.localTmpPath, dirPath, filename), i, total, cid)
		_, err := os.Stat(partPath)
		if err != nil {
			return false
		}
	}
	return true
}

func (u *UserVolume) MakeDir(dir, newDirname string) (elfinder.FileDir, error) {
	var rest elfinder.FileDir
	if dir == "" || dir == "/" {
		dir = u.basePath
	}
	if dir == u.basePath {
		return rest, os.ErrPermission
	}
	pathNames := strings.Split(strings.TrimPrefix(dir, "/"), "/")
	hostDir, ok := u.hosts[pathNames[1]]
	if !ok {
		return rest, os.ErrNotExist
	}
	if hostDir.hostPath == dir {
		return rest, os.ErrPermission
	}
	if hostDir.suMaps == nil {
		hostDir.suMaps = make(map[string]*sysUserVolume)
		systemUsers := hostDir.asset.SystemUsers
		for i, sysUser := range systemUsers {
			hostDir.suMaps[sysUser.Name] = &sysUserVolume{
				VID:        u.ID(),
				hostpath:   hostDir.hostPath,
				suPath:     filepath.Join(hostDir.hostPath, sysUser.Name),
				systemUser: &systemUsers[i],
				rootPath:   u.rootPath,
			}
		}
	}
	sysUserDir, ok := hostDir.suMaps[pathNames[2]]
	if !ok {
		return rest, os.ErrNotExist
	}
	realPath := sysUserDir.ParsePath(dir)
	if sysUserDir.client == nil {
		sftClient, conn, err := u.GetSftpClient(hostDir.asset, sysUserDir.systemUser)
		if err != nil {
			return rest, os.ErrPermission
		}
		sysUserDir.client = sftClient
		sysUserDir.conn = conn
	}
	realDirPath := filepath.Join(realPath, newDirname)
	err := sysUserDir.client.MkdirAll(realDirPath)
	if err != nil {
		return rest, err
	}
	return u.Info(filepath.Join(dir, newDirname))
}

func (u *UserVolume) MakeFile(dir, newFilename string) (elfinder.FileDir, error) {
	var rest elfinder.FileDir
	if dir == "" || dir == "/" {
		dir = u.basePath
	}
	if dir == u.basePath {
		return rest, os.ErrPermission
	}
	pathNames := strings.Split(strings.TrimPrefix(dir, "/"), "/")
	hostDir, ok := u.hosts[pathNames[1]]
	if !ok {
		return rest, os.ErrNotExist
	}
	if hostDir.hostPath == dir {
		return rest, os.ErrPermission
	}
	if hostDir.suMaps == nil {
		hostDir.suMaps = make(map[string]*sysUserVolume)
		systemUsers := hostDir.asset.SystemUsers
		for i, sysUser := range systemUsers {
			hostDir.suMaps[sysUser.Name] = &sysUserVolume{
				VID:        u.ID(),
				hostpath:   hostDir.hostPath,
				suPath:     filepath.Join(hostDir.hostPath, sysUser.Name),
				systemUser: &systemUsers[i],
				rootPath:   u.rootPath,
			}
		}
	}
	sysUserDir, ok := hostDir.suMaps[pathNames[2]]
	if !ok {
		return rest, os.ErrNotExist
	}
	realPath := sysUserDir.ParsePath(dir)
	if sysUserDir.client == nil {
		sftClient, conn, err := u.GetSftpClient(hostDir.asset, sysUserDir.systemUser)
		if err != nil {
			return rest, os.ErrPermission
		}
		sysUserDir.client = sftClient
		sysUserDir.conn = conn

	}
	realFilePath := filepath.Join(realPath, newFilename)
	_, err := sysUserDir.client.Create(realFilePath)
	if err != nil {
		return rest, err
	}

	return u.Info(filepath.Join(dir, newFilename))
}

func (u *UserVolume) Rename(oldNamePath, newname string) (elfinder.FileDir, error) {
	var rest elfinder.FileDir
	pathNames := strings.Split(strings.TrimPrefix(oldNamePath, "/"), "/")
	hostDir, ok := u.hosts[pathNames[1]]
	if !ok {
		return rest, os.ErrNotExist
	}
	if hostDir.suMaps == nil {
		hostDir.suMaps = make(map[string]*sysUserVolume)
		systemUsers := hostDir.asset.SystemUsers
		for i, sysUser := range systemUsers {
			hostDir.suMaps[sysUser.Name] = &sysUserVolume{
				VID:        u.ID(),
				hostpath:   hostDir.hostPath,
				suPath:     filepath.Join(hostDir.hostPath, sysUser.Name),
				systemUser: &systemUsers[i],
				rootPath:   u.rootPath,
			}
		}
	}
	sysUserDir, ok := hostDir.suMaps[pathNames[2]]
	if !ok {
		return rest, os.ErrNotExist
	}
	if sysUserDir.suPath == oldNamePath {
		return rest, os.ErrPermission
	}

	realPath := sysUserDir.ParsePath(oldNamePath)
	if sysUserDir.client == nil {
		sftClient, conn, err := u.GetSftpClient(hostDir.asset, sysUserDir.systemUser)
		if err != nil {
			return rest, os.ErrPermission
		}
		sysUserDir.client = sftClient
		sysUserDir.conn = conn

	}
	dirpath := filepath.Dir(realPath)
	newFilePath := filepath.Join(dirpath, newname)

	err := sysUserDir.client.Rename(oldNamePath, newFilePath)
	if err != nil {
		return rest, err
	}
	return u.Info(newFilePath)
}

func (u *UserVolume) Remove(path string) error {
	if path == "" || path == "/" {
		path = u.basePath
	}
	if path == u.basePath {
		return os.ErrPermission
	}
	pathNames := strings.Split(strings.TrimPrefix(path, "/"), "/")
	hostDir, ok := u.hosts[pathNames[1]]
	if !ok {
		return os.ErrNotExist
	}
	if hostDir.suMaps == nil {
		hostDir.suMaps = make(map[string]*sysUserVolume)
		systemUsers := hostDir.asset.SystemUsers
		for i, sysUser := range systemUsers {
			hostDir.suMaps[sysUser.Name] = &sysUserVolume{
				VID:        u.ID(),
				hostpath:   hostDir.hostPath,
				suPath:     filepath.Join(hostDir.hostPath, sysUser.Name),
				systemUser: &systemUsers[i],
				rootPath:   u.rootPath,
			}
		}
	}
	sysUserDir, ok := hostDir.suMaps[pathNames[2]]
	if !ok {
		return os.ErrNotExist
	}
	if sysUserDir.suPath == path {
		return os.ErrPermission
	}
	realPath := sysUserDir.ParsePath(path)
	if sysUserDir.client == nil {
		sftClient, conn, err := u.GetSftpClient(hostDir.asset, sysUserDir.systemUser)
		if err != nil {
			return os.ErrPermission
		}
		sysUserDir.client = sftClient
		sysUserDir.conn = conn

	}
	return sysUserDir.client.Remove(realPath)
}

func (u *UserVolume) Paste(dir, filename, suffix string, reader io.ReadCloser) (elfinder.FileDir, error) {
	var rest elfinder.FileDir
	if dir == "" || dir == "/" {
		dir = u.basePath
	}
	if dir == u.basePath {
		return rest, os.ErrPermission
	}
	pathNames := strings.Split(strings.TrimPrefix(dir, "/"), "/")
	hostDir, ok := u.hosts[pathNames[1]]
	if !ok {
		return rest, os.ErrNotExist
	}
	if hostDir.hostPath == dir {
		return rest, os.ErrPermission
	}
	if hostDir.suMaps == nil {
		hostDir.suMaps = make(map[string]*sysUserVolume)
		systemUsers := hostDir.asset.SystemUsers
		for i, sysUser := range systemUsers {
			hostDir.suMaps[sysUser.Name] = &sysUserVolume{
				VID:        u.ID(),
				hostpath:   hostDir.hostPath,
				suPath:     filepath.Join(hostDir.hostPath, sysUser.Name),
				systemUser: &systemUsers[i],
				rootPath:   u.rootPath,
			}
		}
	}
	sysUserDir, ok := hostDir.suMaps[pathNames[2]]
	if !ok {
		return rest, os.ErrNotExist
	}
	realPath := sysUserDir.ParsePath(dir)
	if sysUserDir.client == nil {
		sftClient, conn, err := u.GetSftpClient(hostDir.asset, sysUserDir.systemUser)
		if err != nil {
			return rest, os.ErrPermission
		}
		sysUserDir.client = sftClient
		sysUserDir.conn = conn
	}

	realFilePath := filepath.Join(realPath, filename)
	_, err := sysUserDir.client.Stat(realFilePath)
	if err == nil {
		realPath += suffix
	}
	fd, err := sysUserDir.client.OpenFile(realPath, os.O_RDWR|os.O_CREATE)
	if err != nil {
		return rest, err
	}
	defer fd.Close()
	_, err = io.Copy(fd, reader)
	if err != nil {
		return rest, err
	}
	return u.Info(realPath)
}

func (u *UserVolume) RootFileDir() elfinder.FileDir {
	var resFDir = elfinder.FileDir{}
	resFDir.Name = u.Name
	resFDir.Hash = hashPath(u.Uuid, u.basePath)
	resFDir.Mime = "directory"
	resFDir.Volumeid = u.Uuid
	resFDir.Dirs = 1
	resFDir.Read, resFDir.Write = 1, 1
	resFDir.Locked = 1
	return resFDir
}

func (u *UserVolume) GetSftpClient(asset *model.Asset, sysUser *model.SystemUser) (sftpClient *sftp.Client, sshClient *gossh.Client, err error) {
	sshClient, err = srvconn.NewClient(u.user, asset, sysUser, config.GetConf().SSHTimeout*time.Second)
	if err != nil {
		return
	}
	sftpClient, err = sftp.NewClient(sshClient)
	if err != nil {
		return
	}
	return sftpClient, sshClient, nil
}

func (u *UserVolume) Close() {
	for _, host := range u.hosts {
		if host.suMaps == nil {
			continue
		}
		for _, su := range host.suMaps {
			su.Close()
		}
	}
}

type hostnameVolume struct {
	VID      string
	homePath string
	hostPath string // /home/hostname/
	time     time.Time
	asset    *model.Asset
	suMaps   map[string]*sysUserVolume
}

func (h *hostnameVolume) info() elfinder.FileDir {
	var resFDir = elfinder.FileDir{}
	resFDir.Name = h.asset.Hostname
	resFDir.Hash = hashPath(h.VID, h.hostPath)
	resFDir.Phash = hashPath(h.VID, h.homePath)
	resFDir.Mime = "directory"
	resFDir.Volumeid = h.VID
	resFDir.Dirs = 1
	resFDir.Read, resFDir.Write = 1, 1
	return resFDir
}

type sysUserVolume struct {
	VID        string
	hostpath   string
	suPath     string
	rootPath   string
	systemUser *model.SystemUser

	client *sftp.Client
	conn   *gossh.Client
}

func (su *sysUserVolume) info() elfinder.FileDir {
	var resFDir = elfinder.FileDir{}
	resFDir.Name = su.systemUser.Name
	resFDir.Hash = hashPath(su.VID, su.suPath)
	resFDir.Phash = hashPath(su.VID, su.hostpath)
	resFDir.Mime = "directory"
	resFDir.Volumeid = su.VID
	resFDir.Dirs = 1
	resFDir.Read, resFDir.Write = 1, 1
	return resFDir
}

func (su *sysUserVolume) ParsePath(path string) string {
	realPath := strings.ReplaceAll(path, su.suPath, su.rootPath)
	logger.Debug("real path: ", realPath)
	return realPath
}

func (su *sysUserVolume) Close() {
	if su.client != nil {
		_ = su.client.Close()
		su.client = nil
	}
	srvconn.RecycleClient(su.conn)
}

func hashPath(id, path string) string {
	return elfinder.CreateHash(id, path)
}
