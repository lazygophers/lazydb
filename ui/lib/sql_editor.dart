// SQL 编辑器（#21）：re_editor + re_highlight 的 SQL 高亮，
// 补全供词来自后端 schema 缓存（表名 + 表.字段 + 关键字）。
import 'package:flutter/material.dart';
import 'package:re_editor/re_editor.dart';
import 'package:re_highlight/languages/sql.dart';
import 'package:re_highlight/styles/atom-one-dark.dart';

/// 补全供词表。表名/字段由调用方（界面层经 HTTP）异步灌入；
/// `表.` 触发字段补全，未加载的表回调 [onTableMiss] 异步补灌（下次输入生效）。
class SqlWords implements CodeAutocompletePromptsBuilder {
  SqlWords({this.onTableMiss});

  void Function(String table)? onTableMiss;
  final _tables = <CodePrompt>[];
  final _columnsOf = <String, List<CodePrompt>>{};
  final _fetched = <String>{};
  static final _keywords = _sqlKeywords();

  void setTables(Iterable<String> names) {
    _tables
      ..clear()
      ..addAll(names.map((n) => CodeKeywordPrompt(word: n)));
    _columnsOf.clear();
    _fetched.clear();
  }

  void setColumns(String table, Iterable<Map<String, dynamic>> cols) {
    _fetched.add(table);
    _columnsOf[table] = cols
        .map((c) => CodeFieldPrompt(
            word: '${c['name']}', type: '${c['type']}'))
        .toList();
  }

  /// 供测试直接喂「一行文本 + 光标位」。
  CodeAutocompleteEditingValue? buildFor(String line, int caret) =>
      _build(line, caret);

  @override
  CodeAutocompleteEditingValue? build(
      BuildContext context, CodeLine codeLine, CodeLineSelection selection) {
    return _build(codeLine.text, selection.extentOffset);
  }

  CodeAutocompleteEditingValue? _build(String text, int caret) {
    final before = text.substring(0, caret);
    if (before.isEmpty) return null;
    final after = text.substring(caret);
    // 光标在字符串里不出提示
    if ((before.contains("'") || before.contains('"')) &&
        (after.contains("'") || after.contains('"'))) {
      return null;
    }

    final Iterable<CodePrompt> prompts;
    final String input;
    if (before.endsWith('.')) {
      final table = _wordBefore(before, before.length - 1);
      if (!_fetched.contains(table)) onTableMiss?.call(table);
      prompts = _columnsOf[table] ?? const [];
      input = '';
    } else {
      input = _wordBefore(before, before.length);
      if (input.isEmpty) return null;
      prompts = [..._keywords, ..._tables].where((p) => p.match(input));
    }
    if (prompts.isEmpty) return null;
    return CodeAutocompleteEditingValue(
        input: input, prompts: prompts.toList(), index: 0);
  }

  static String _wordBefore(String s, int end) {
    var start = end - 1;
    while (start >= 0 && _isWordChar(s.codeUnitAt(start))) {
      start--;
    }
    return s.substring(start + 1, end);
  }

  static bool _isWordChar(int c) =>
      (c >= 0x30 && c <= 0x39) ||
      (c >= 0x41 && c <= 0x5A) ||
      (c >= 0x61 && c <= 0x7A) ||
      c == 0x5F; // _

  static List<CodePrompt> _sqlKeywords() {
    final out = <CodePrompt>{};
    for (final group in (langSql.keywords as Map?)?.values ?? const []) {
      if (group is! List) continue;
      out.addAll(group.map((k) => CodeKeywordPrompt(word: '$k')));
    }
    return out.toList();
  }
}

/// 高亮 + 行号 + 补全弹层的成品编辑器。
class SqlEditor extends StatelessWidget {
  const SqlEditor({super.key, required this.controller, required this.words});

  final CodeLineEditingController controller;
  final SqlWords words;

  @override
  Widget build(BuildContext context) {
    return CodeAutocomplete(
      viewBuilder: (context, notifier, onSelected) =>
          _PromptOverlay(notifier: notifier, onSelected: onSelected),
      promptsBuilder: words,
      child: CodeEditor(
        controller: controller,
        hint: 'SQL（Cmd/Ctrl+Enter 执行；选中片段优先）',
        style: CodeEditorStyle(
          fontSize: 13,
          fontFamily: 'monospace',
          backgroundColor: Color(0xff282c34),
          codeTheme: CodeHighlightTheme(
            languages: {
              'sql': CodeHighlightThemeMode(mode: langSql),
            },
            theme: atomOneDarkTheme,
          ),
        ),
        indicatorBuilder: (context, editingController, chunkController, notifier) =>
            DefaultCodeLineNumber(
                controller: editingController, notifier: notifier),
      ),
    );
  }
}

class _PromptOverlay extends StatelessWidget implements PreferredSizeWidget {
  const _PromptOverlay({required this.notifier, required this.onSelected});

  final ValueNotifier<CodeAutocompleteEditingValue> notifier;
  final ValueChanged<CodeAutocompleteResult> onSelected;

  @override
  Size get preferredSize => const Size.fromHeight(160);

  @override
  Widget build(BuildContext context) {
    return Material(
      elevation: 4,
      borderRadius: BorderRadius.circular(4),
      color: const Color(0xff21252b),
      child: ValueListenableBuilder<CodeAutocompleteEditingValue>(
        valueListenable: notifier,
        builder: (context, value, _) => ListView.builder(
          itemCount: value.prompts.length,
          itemBuilder: (context, i) {
            final p = value.prompts[i];
            final type = p is CodeFieldPrompt ? p.type : 'keyword';
            final selected = i == value.index;
            return ListTile(
              dense: true,
              selected: selected,
              title: Text(p.word,
                  style: const TextStyle(
                      fontFamily: 'monospace', fontSize: 12)),
              trailing: Text(type,
                  style: const TextStyle(fontSize: 10, color: Colors.grey)),
              onTap: () => onSelected(value.prompts[i].autocomplete),
            );
          },
        ),
      ),
    );
  }
}
