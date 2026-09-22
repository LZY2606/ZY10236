package dbase

// Flush forces any buffered writes of the DBF file and its associated FPT
// memo file down to the underlying storage, when the handles support it
// (os.File on Unix/Windows, or any handle implementing Sync() error).
//
// Flush is durable-per-file only: the DBF and the FPT are two separate files
// and the operating system provides no joint transaction, so a power loss
// between the two Sync calls can still leave a free-list gap. See
// File.CheckIntegrity for diagnosing that state.
//
// Handles that do not implement Sync are skipped without error, which keeps
// GenericIO usable with purely in-memory io.ReadWriteSeeker implementations.
func (file *File) Flush() error {
	if file == nil {
		return NewError("cannot flush a nil table")
	}
	if err := syncHandle(file.handle, "DBF"); err != nil {
		return err
	}
	if err := syncHandle(file.relatedHandle, "FPT"); err != nil {
		return err
	}
	return nil
}
