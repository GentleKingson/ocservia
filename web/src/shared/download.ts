export function downloadBlob(blob: Blob, filename: string): void {
  const link = document.createElement("a");
  const url = URL.createObjectURL(blob);
  link.href = url;
  link.download = filename;
  try {
    link.click();
  } finally {
    setTimeout(() => {
      URL.revokeObjectURL(url);
    }, 1_000);
  }
}
