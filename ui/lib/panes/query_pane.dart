// SQL + 结果面板（#28 从 main.dart 拆出）：编辑器、执行/导出/历史、结果/DDL 展示。
import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:re_editor/re_editor.dart';

import '../backend.dart';
import '../result_grid.dart';
import '../sql_editor.dart';

class QueryPane extends StatelessWidget {
  const QueryPane({
    super.key,
    required this.be,
    required this.sql,
    required this.words,
    required this.selectedConn,
    required this.running,
    required this.exporting,
    required this.result,
    required this.runError,
    required this.ddlText,
    required this.onRun,
    required this.onExport,
  });

  final Backend be;
  final CodeLineEditingController sql;
  final SqlWords words;
  final String? selectedConn;
  final bool running;
  final bool exporting;
  final QueryResult? result;
  final String runError;
  final String? ddlText;
  final Future<void> Function() onRun;
  final Future<void> Function(String format) onExport;

  @override
  Widget build(BuildContext context) {
    return Column(crossAxisAlignment: CrossAxisAlignment.stretch, children: [
      SizedBox(
        height: 220,
        child: CallbackShortcuts(
          bindings: {
            const SingleActivator(LogicalKeyboardKey.enter, meta: true): onRun,
            const SingleActivator(LogicalKeyboardKey.enter, control: true):
                onRun,
          },
          child: SqlEditor(controller: sql, words: words),
        ),
      ),
      Padding(
        padding: const EdgeInsets.all(8),
        child: Row(children: [
          Text(selectedConn == null ? '未选连接' : '整段执行；有选中则只执行选中',
              style: const TextStyle(fontSize: 11, color: Colors.grey)),
          const Spacer(),
          IconButton(
              tooltip: '查询历史',
              icon: const Icon(Icons.history, size: 18),
              onPressed: () => showHistoryDialog(context, be, sql)),
          const SizedBox(width: 8),
          PopupMenuButton<String>(
            tooltip: '导出',
            enabled: selectedConn != null && !exporting && !running,
            onSelected: onExport,
            itemBuilder: (_) => const [
              PopupMenuItem(value: 'csv', child: Text('导出 CSV')),
              PopupMenuItem(value: 'xlsx', child: Text('导出 Excel')),
            ],
          ),
          const SizedBox(width: 8),
          FilledButton(
            onPressed: selectedConn == null || running ? null : onRun,
            child: running
                ? const SizedBox(
                    width: 14,
                    height: 14,
                    child: CircularProgressIndicator(strokeWidth: 2))
                : const Text('执行'),
          ),
        ]),
      ),
      if (runError.isNotEmpty)
        Padding(
          padding: const EdgeInsets.symmetric(horizontal: 8),
          child: Text(runError, style: const TextStyle(color: Colors.redAccent)),
        ),
      Expanded(
        child: ddlText != null
            ? SingleChildScrollView(
                padding: const EdgeInsets.all(8),
                child: SelectableText(ddlText!,
                    style: const TextStyle(fontFamily: 'monospace', fontSize: 12)))
            : result != null
                ? ResultGrid(result: result!)
                : const Center(
                    child: Text('选表看结构，或输入 SQL 执行',
                        style: TextStyle(color: Colors.grey))),
      ),
      if (result != null)
        Padding(
          padding: const EdgeInsets.symmetric(horizontal: 8, vertical: 2),
          child: Text(
              '${result!.rows.length} 行${result!.truncated ? '（已截断）' : ''}',
              style: const TextStyle(fontSize: 11, color: Colors.grey)),
        ),
    ]);
  }
}

// 查询历史对话框（#23 原在 main.dart）：点历史语句取回编辑器。
Future<void> showHistoryDialog(
    BuildContext context, Backend be, CodeLineEditingController sql) async {
  final q = TextEditingController();
  await showDialog(
    context: context,
    builder: (context) => StatefulBuilder(
      builder: (context, setDialog) {
        Future<List<dynamic>> items() => be.history(q.text);
        return AlertDialog(
          title: const Text('查询历史'),
          content: SizedBox(
            width: 640,
            height: 420,
            child: Column(children: [
              TextField(
                controller: q,
                decoration: const InputDecoration(
                    hintText: '按语句/连接/报错关键字搜',
                    isDense: true,
                    prefixIcon: Icon(Icons.search, size: 18)),
                onChanged: (_) => setDialog(() {}),
              ),
              const SizedBox(height: 8),
              Expanded(
                child: FutureBuilder<List<dynamic>>(
                  future: items(),
                  builder: (context, snap) {
                    if (!snap.hasData) {
                      return const Center(
                          child: SizedBox(
                              width: 18,
                              height: 18,
                              child: CircularProgressIndicator(strokeWidth: 2)));
                    }
                    if (snap.data!.isEmpty) {
                      return const Center(
                          child: Text('无记录', style: TextStyle(color: Colors.grey)));
                    }
                    return ListView.builder(
                      itemCount: snap.data!.length,
                      itemBuilder: (context, i) {
                        final e = snap.data![i] as Map<String, dynamic>;
                        final dt = DateTime.fromMillisecondsSinceEpoch(e['ts']);
                        return ListTile(
                          dense: true,
                          title: Text('${e['sql']}',
                              maxLines: 1,
                              overflow: TextOverflow.ellipsis,
                              style: const TextStyle(fontFamily: 'monospace', fontSize: 12)),
                          subtitle: Text(
                              '${dt.toLocal()}  连接 ${e['conn']}  ${e['ms']}ms  ${e['ok'] == true ? '成功' : '失败：${e['error'] ?? ''}'}',
                              style: const TextStyle(fontSize: 10)),
                          onTap: () {
                            sql.text = e['sql'] as String; // 点历史语句取回编辑器
                            Navigator.pop(context);
                          },
                        );
                      },
                    );
                  },
                ),
              ),
            ]),
          ),
          actions: [
            TextButton(
                onPressed: () => Navigator.pop(context),
                child: const Text('关闭')),
          ],
        );
      },
    ),
  );
  q.dispose();
}
