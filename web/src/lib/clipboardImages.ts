/** Read only image files; never interpret clipboard HTML or fetch remote URLs. */
export function pastedImages(data: Pick<DataTransfer, 'files' | 'items'> | null): File[] {
  if (!data) return []
  const files = Array.from(data.files).filter(file => file.type.startsWith('image/'))
  if (files.length) return files
  return Array.from(data.items).flatMap(item => {
    if (item.kind !== 'file' || !item.type.startsWith('image/')) return []
    const file = item.getAsFile()
    return file ? [file] : []
  })
}

/** Capture before xterm can paste mixed image/text data into a shell. */
export function captureImagePaste(event: ClipboardEvent, receive: (files: File[]) => void): boolean {
  const files = pastedImages(event.clipboardData)
  if (!files.length) return false
  event.preventDefault()
  event.stopImmediatePropagation()
  receive(files)
  return true
}

export async function readClipboardImages(clipboard: Pick<Clipboard, 'read'> | undefined): Promise<File[]> {
  if (!clipboard?.read) throw new Error('浏览器不支持读取图片，请用粘贴快捷键或“选择图片”。')
  const items = await clipboard.read()
  const files: File[] = []
  for (const item of items) {
    const type = item.types.find(type => type.startsWith('image/'))
    if (!type) continue
    const blob = await item.getType(type)
    const extension = ({ 'image/jpeg': 'jpg', 'image/svg+xml': 'svg', 'image/x-icon': 'ico' } as Record<string, string>)[type]
      ?? (type.slice(6).replace(/[^a-z0-9]/gi, '').slice(0, 12) || 'img')
    files.push(new File([blob], `pasted-image-${files.length + 1}.${extension}`, { type }))
  }
  return files
}
