# New projects and files (Agent v0.1.24)

On a device page, select **新建项目 / 文件**. Create a project/folder with an optional UTF-8 starter file and optional Git initialization, or create a file directly in an existing allowed directory. Choose an authorized installed CLI to open it, or choose creation only. The parent may be an allowed root or a subdirectory; Agent path and private-state checks remain authoritative. Project names and file basenames reject traversal, separators and Windows device names. Initial text is limited to 64 KiB. Existing directories/files are never overwritten.

New project paths become the working directory, are remembered, and are saved as project preferences when an available CLI can be associated. Creation does not require an initial Git commit; independent worktree creation remains a separate action. Partial optional Git/file setup is reported rather than silently deleted or retried. The user can continue opening the CLI after reviewing a real warning.

Session file management includes **新建文本文件**, UTF-8 content up to 256 KiB, atomic chunked upload without overwrite, and preview after saving. **交给编程助手** routes to the device's CLI selector using the file's parent directory. Recognized programming CLI sessions receive a file-reference input draft; it is not submitted automatically. Shell sessions do not receive a natural-language command containing a filename. CLI installation and authorization remain managed by the existing device tool controls.

The Server gates project creation on Agent capability, verifies device ownership and routes only to its owner. Agent roots include symlink/private-state checks. Creating a file/directory alone creates no terminal session and makes no model call.
