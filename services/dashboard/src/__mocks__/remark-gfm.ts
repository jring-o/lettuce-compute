// Mock for remark-gfm — returns a no-op plugin
export default function remarkGfm() {
  return (tree: unknown) => tree;
}
