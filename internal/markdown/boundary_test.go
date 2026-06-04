package markdown

import "testing"

func TestFindSafeCut(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    int // expected safe cut offset
	}{
		{
			name:    "empty content",
			content: "",
			want:    0,
		},
		{
			name:    "plain paragraphs - all safe",
			content: "第一段\n第二段\n第三段\n",
			want:    len("第一段\n第二段\n第三段\n"),
		},
		{
			name:    "unclosed code block - cut before fence",
			content: "说明：\n```python\ndef hello():\n    print",
			want:    len("说明：\n"),
		},
		{
			name:    "closed code block followed by text - all safe",
			content: "```python\ncode\n```\n结果如下\n",
			want:    len("```python\ncode\n```\n结果如下\n"),
		},
		{
			name:    "no safe cut - entire content in code block",
			content: "```python\ndef hello():",
			want:    0,
		},
		{
			name:    "table sequence - cut before table",
			content: "标题\n| A | B |\n|---|---|\n| 1 |",
			want:    len("标题\n"),
		},
		{
			name:    "closed table followed by text - all safe",
			content: "| A | B |\n|---|---|\n| 1 | 2 |\n\n总结\n",
			want:    len("| A | B |\n|---|---|\n| 1 | 2 |\n\n总结\n"),
		},
		{
			name:    "code fence with info string",
			content: "```\ncode\n```\nafter\n",
			want:    len("```\ncode\n```\nafter\n"),
		},
		{
			name:    "tilde fence",
			content: "~~~\ncode\n~~~\nafter\n",
			want:    len("~~~\ncode\n~~~\nafter\n"),
		},
		{
			name:    "single line",
			content: "just a line",
			want:    len("just a line"),
		},
		{
			name:    "heading is safe",
			content: "## Implementation\n",
			want:    len("## Implementation\n"),
		},
		{
			name:    "list items are safe",
			content: "- item 1\n- item 2\n",
			want:    len("- item 1\n- item 2\n"),
		},
		{
			name:    "code block with backticks inside",
			content: "```\n含有 ``` 的代码\n```\nafter\n",
			want:    len("```\n含有 ``` 的代码\n```\nafter\n"),
		},
		{
			name:    "HTML block open only",
			content: "before\n<div>\ncontent",
			want:    len("before\n"),
		},
		{
			name:    "HTML block open and close",
			content: "<div>\ncontent\n</div>\nafter\n",
			want:    len("<div>\ncontent\n</div>\nafter\n"),
		},
		{
			name:    "table separator only row",
			content: "before\n|---|---|\n| a | b |\nafter\n",
			want:    len("before\n|---|---|\n| a | b |\nafter\n"),
		},
		{
			name:    "unclosed table at end",
			content: "intro\n| a | b |\n|---|---|\n| 1 |",
			want:    len("intro\n"),
		},
		{
			name:    "multiple code blocks",
			content: "```js\ncode1\n```\ntext\n```py\ncode2\n```\nend\n",
			want:    len("```js\ncode1\n```\ntext\n```py\ncode2\n```\nend\n"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FindSafeCut(tt.content)
			if got != tt.want {
				t.Errorf("FindSafeCut() = %d (want %d)\ncontent: %q\ngot slice: %q",
					got, tt.want, tt.content, tt.content[:got])
			}
		})
	}
}
