export function truncateLabel(label: string, maxLength = 12): string {
  if (label.length <= maxLength) return label;
  return `${label.slice(0, maxLength - 3)}...`;
}
