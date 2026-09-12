// lazydb 桌面界面（#20 拆分 #28）：页面骨架 + 各面板状态协调。
// 界面零业务逻辑：所有数据经 Backend 的 HTTP API（ADR-0001）。
import 'dart:ui' show AppExitResponse;

import 'package:file_selector/file_selector.dart';
import 'package:flutter/material.dart';
import 'package:re_editor/re_editor.dart';

import 'backend.dart';
import 'panes/conns_pane.dart';
import 'panes/query_pane.dart';
import 'panes/tree_pane.dart';
import 'result_grid.dart';
import 'sql_editor.dart';

void main() {
  runApp(const LazyDbApp());
}

class LazyDbApp extends StatelessWidget {
  const LazyDbApp({super.key});

  @override
  Widget build(BuildContext context) {
    return MaterialApp(
      title: 'lazydb',
      theme: ThemeData(
          useMaterial3: true,
          colorScheme: ColorScheme.fromSeed(
              seedColor: const Color(0xFF4F8CC9), brightness: Brightness.dark)),
      home: const HomePage(),
    );
  }
}

class HomePage extends StatefulWidget {
  const HomePage({super.key});

  @override
  State<HomePage> createState() => _HomePageState();
}

class _HomePageState extends State<HomePage> with WidgetsBindingObserver {
  Backend? be;
  String? bootError;
  List<dynamic> conns = [];
  final trees = <String, List<TreeNode>>{}; // connID → 根层节点
  String? selectedConn;

  QueryResult? result;
  String runError = '';
  bool running = false;
  String? ddlText;
  final sql = CodeLineEditingController.fromText('');
  final words = SqlWords();
  final tableDb = <String, String>{}; // 表名 → 库名（补全取字段用）
  bool exporting = false;

  @override
  void initState() {
    super.initState();
    WidgetsBinding.instance.addObserver(this);
    words.onTableMiss = _fetchColumns;
    _boot();
  }

  // 设置「保留核心」关着时，退出界面连带退出后端（#29 / ADR-0007）。
  @override
  Future<AppExitResponse> didRequestAppExit() async {
    final b = be;
    if (b != null) {
      try {
        final s = await b.getSettings();
        if (s['keep_core_on_close'] != true) await b.shutdown();
      } catch (_) {/* 后端不在了正好 */}
    }
    return AppExitResponse.exit;
  }

  @override
  void dispose() {
    WidgetsBinding.instance.removeObserver(this);
    sql.dispose();
    super.dispose();
  }

  Future<void> _boot() async {
    setState(() {
      be = null;
      bootError = null;
    });
    try {
      final b = await Backend.attach(Backend.findBinary());
      setState(() => be = b);
      await _reload();
    } catch (e) {
      setState(() => bootError = '$e');
    }
  }

  Future<void> _reload() async {
    final list = await be!.listConnections();
    setState(() => conns = list);
    final ids = list.map((c) => c['id'] as String).toSet();
    trees.removeWhere((id, _) => !ids.contains(id));
  }

  Future<void> _deleteConn(String id) async {
    await be!.deleteConnection(id);
    trees.remove(id);
    if (selectedConn == id) setState(() => selectedConn = null);
    await _reload();
  }

  Future<void> _runSql() async {
    if (selectedConn == null) return;
    final text = sql.selectedText.trim().isNotEmpty ? sql.selectedText : sql.text;
    if (text.trim().isEmpty) return;
    setState(() {
      running = true;
      runError = '';
      ddlText = null;
    });
    try {
      result = QueryResult.fromJson(
          await be!.exec(selectedConn!, text) as Map<String, dynamic>);
    } catch (e) {
      result = null;
      runError = '$e';
    } finally {
      setState(() => running = false);
    }
  }

  // 导出当前语句结果（#24）：存盘面板选位置，后端流式下载。
  Future<void> _export(String format) async {
    if (selectedConn == null || running) return;
    final text = sql.selectedText.trim().isNotEmpty ? sql.selectedText : sql.text;
    if (text.trim().isEmpty) return;
    final loc = await getSaveLocation(
        suggestedName: 'export.$format',
        acceptedTypeGroups: [
          XTypeGroup(label: format.toUpperCase(), extensions: [format])
        ]);
    if (loc == null) return;
    setState(() => exporting = true);
    try {
      await be!.exportToFile(selectedConn!, text, format, loc.path);
    } catch (e) {
      setState(() => runError = '导出失败：$e');
    } finally {
      setState(() => exporting = false);
    }
  }

  // ---- 补全供词（全走缓存接口，断网时后端 stale-fallback 仍供词） ----

  Future<void> _loadWords() async {
    if (selectedConn == null) return;
    try {
      final tables = <String>[];
      tableDb.clear();
      for (final db in await be!.children(selectedConn!, [])) {
        for (final t in await be!.children(selectedConn!, [db['name']])) {
          tables.add('${t['name']}');
          tableDb['${t['name']}'] = '${db['name']}';
        }
      }
      words.setTables(tables);
    } catch (_) {/* 供词失败不拦主流程 */}
  }

  Future<void> _fetchColumns(String table) async {
    final db = tableDb[table];
    if (db == null) return;
    try {
      final r = await be!.columns(selectedConn!, [db, table]);
      words.setColumns(table, (r['columns'] as List).cast<Map<String, dynamic>>());
    } catch (_) {/* 同上 */}
  }

  // 树面板回调：DDL 展示归结果区（tree_pane 只发请求）
  Future<void> _showDdl(String connId, TreeNode n) async {
    setState(() {
      ddlText = '加载中…';
      result = null;
    });
    try {
      final ddl = await be!.ddl(connId, n.path);
      setState(() => ddlText = ddl);
    } catch (e) {
      setState(() => ddlText = '$e');
    }
  }

  // ---- 布局 ----

  @override
  Widget build(BuildContext context) {
    if (bootError != null) {
      return Scaffold(
        body: Center(
          child: Column(mainAxisSize: MainAxisSize.min, children: [
            Text('后端不可用：$bootError'),
            const SizedBox(height: 8),
            FilledButton(onPressed: _boot, child: const Text('重试')),
          ]),
        ),
      );
    }
    if (be == null) {
      return const Scaffold(body: Center(child: Text('连接后端…')));
    }
    return Scaffold(
      body: Row(children: [
        SizedBox(
            width: 230,
            child: ConnsPane(
              be: be!,
              conns: conns,
              selectedConn: selectedConn,
              onSelect: (id) {
                setState(() => selectedConn = id);
                _loadWords();
              },
              onDelete: _deleteConn,
              onSaved: _reload,
            )),
        const VerticalDivider(width: 1),
        SizedBox(width: 300, child: _treePane()),
        const VerticalDivider(width: 1),
        Expanded(child: _queryPane()),
      ]),
    );
  }

  Widget _treePane() {
    if (selectedConn == null) {
      return const Center(
          child: Text('先选一个连接', style: TextStyle(color: Colors.grey)));
    }
    final c = conns.firstWhere((x) => x['id'] == selectedConn)
        as Map<String, dynamic>;
    return TreePane(
      be: be!,
      connId: selectedConn!,
      connName: '${c['name']}',
      trees: trees,
      onColumns: words.setColumns,
      onDdl: _showDdl,
      onPickSql: (s) => sql.text = s,
    );
  }

  Widget _queryPane() {
    return QueryPane(
      be: be!,
      sql: sql,
      words: words,
      selectedConn: selectedConn,
      running: running,
      exporting: exporting,
      result: result,
      runError: runError,
      ddlText: ddlText,
      onRun: _runSql,
      onExport: _export,
    );
  }
}
