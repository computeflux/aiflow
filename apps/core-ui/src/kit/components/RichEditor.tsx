import { useRef } from "react";
import JoditEditor from "jodit-react";

interface RichEditorProps {
  value: string;
  onChange: (html: string) => void;
  minHeight?: number;
}

export function RichEditor({ value, onChange, minHeight = 500 }: RichEditorProps) {
  const editorRef = useRef<any>(null);

  return (
    <JoditEditor
      ref={editorRef}
      value={value}
      config={{
        readonly: false,
        height: minHeight,
        toolbarSticky: false,
        buttons: [
          "source", "|",
          "bold", "italic", "underline", "strikethrough", "|",
          "fontsize", "|",
          "ul", "ol", "|",
          "outdent", "indent", "|",
          "align", "|",
          "link", "|",
          "paragraph", "heading1", "heading2", "|",
          "undo", "redo", "|",
          "selectall", "cut", "copy", "paste",
        ],
        showXPathInStatusbar: false,
        showCharsCounter: false,
        showWordsCounter: false,
        defaultActionOnPaste: "insert_clear_html",
        cleanHTML: {},
        askBeforePasteHTML: false,
        askBeforePasteFromWord: false,
      }}
      onBlur={(newContent: string) => onChange(newContent)}
    />
  );
}
