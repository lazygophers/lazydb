// 连接面板（#28 从 main.dart 拆出）：列表 + 新建/编辑/删除 + 退出后端。
import 'dart:io' show exit;

import 'package:flutter/material.dart';

import '../backend.dart';

class ConnsPane extends StatelessWidget {
  const ConnsPane({
    super.key,
    required this.be,
    required this.conns,
    required this.selectedConn,
    required this.onSelect,
    required this.onDelete,
    required this.onSaved,
  });

  final Backend be;
  final List<dynamic> conns;
  final String? selectedConn;
  final void Function(String id) onSelect;
  final Future<void> Function(String id) onDelete; // 删除（含选中态清理）
  final Future<void> Function() onSaved; // 增删改后刷新列表

  @override
  Widget build(BuildContext context) {
    return Column(crossAxisAlignment: CrossAxisAlignment.stretch, children: [
      Padding(
        padding: const EdgeInsets.all(8),
        child: Row(children: [
          const Text('连接'),
          const Spacer(),
          IconButton(
              tooltip: '退出后端（含界面与全部驱动进程）',
              icon: const Icon(Icons.power_settings_new, size: 18),
              onPressed: () async {
                await be.shutdown();
                exit(0);
              }),
          IconButton(
              tooltip: '设置',
              icon: const Icon(Icons.settings, size: 18),
              onPressed: () => _settingsDialog(context)),
          IconButton(
              tooltip: '新建连接',
              icon: const Icon(Icons.add),
              onPressed: () => showConnDialog(context, be, onSaved: onSaved)),
        ]),
      ),
      Expanded(
        child: ListView.builder(
          itemCount: conns.length,
          itemBuilder: (context, i) {
            final c = conns[i] as Map<String, dynamic>;
            final id = c['id'] as String;
            return ListTile(
              dense: true,
              selected: selectedConn == id,
              title: Text('${c['name']}'),
              subtitle: Text('${c['config']['driver']}',
                  style: const TextStyle(fontSize: 11)),
              onTap: () => onSelect(id),
              trailing: PopupMenuButton<String>(
                onSelected: (op) => op == 'edit'
                    ? showConnDialog(context, be,
                        old: c, oldId: id, onSaved: onSaved)
                    : onDelete(id),
                itemBuilder: (_) => const [
                  PopupMenuItem(value: 'edit', child: Text('编辑')),
                  PopupMenuItem(value: 'delete', child: Text('删除')),
                ],
              ),
            );
          },
        ),
      ),
    ]);
  }

  Future<void> _settingsDialog(BuildContext context) async {
    Map<String, dynamic> s;
    try {
      s = await be.getSettings();
    } catch (_) {
      s = {'keep_core_on_close': true};
    }
    if (!context.mounted) return;
    await showDialog(
      context: context,
      builder: (context) => StatefulBuilder(
        builder: (context, setDialog) => AlertDialog(
          title: const Text('设置'),
          content: Column(mainAxisSize: MainAxisSize.min, children: [
            SwitchListTile(
              contentPadding: EdgeInsets.zero,
              title: const Text('关闭界面时保留后端核心'),
              subtitle: const Text(
                  '开：关界面后连接与缓存保活，重开秒进\n关：关界面时后端一并退出（含驱动进程）',
                  style: TextStyle(fontSize: 11)),
              value: s['keep_core_on_close'] == true,
              onChanged: (v) async {
                setDialog(() => s['keep_core_on_close'] = v);
                try {
                  final saved = await be.putSettings(s);
                  setDialog(() => s = saved);
                } catch (_) {/* 保存失败留原值，下次打开重读 */}
              },
            ),
          ]),
          actions: [
            TextButton(
                onPressed: () => Navigator.pop(context),
                child: const Text('关闭')),
          ],
        ),
      ),
    );
  }
}

// 新建/编辑连接对话框（#20/#25）：含 SSH 隧道区块与连通性测试。
Future<void> showConnDialog(BuildContext context, Backend be,
    {Map<String, dynamic>? old, String? oldId, required Future<void> Function() onSaved}) async {
  final name = TextEditingController(text: old?['name'] ?? '');
  final dsn = TextEditingController(text: old?['config']?['dsn'] ?? '');
  var driver = old?['config']?['driver'] ?? 'sqlite';
  final oldSsh = (old?['config']?['ssh'] as Map<String, dynamic>?);
  var useSsh = oldSsh != null;
  var sshPort = (oldSsh?['port'] as num?)?.toInt() ?? 22;
  var targetPort = (oldSsh?['target_port'] as num?)?.toInt() ?? 3306;
  final sshHost = TextEditingController(text: oldSsh?['host'] ?? '');
  final sshUser = TextEditingController(text: oldSsh?['user'] ?? 'root');
  final sshKey = TextEditingController(text: oldSsh?['key_path'] ?? '');
  final sshTarget =
      TextEditingController(text: oldSsh?['target_host'] ?? '127.0.0.1');
  String testMsg = '';
  // 组 config：SSH 区块只在勾选时带上
  Map<String, dynamic> cfg() => {
        'driver': driver,
        'dsn': dsn.text,
        if (useSsh)
          'ssh': {
            'host': sshHost.text,
            'port': sshPort,
            'user': sshUser.text,
            'key_path': sshKey.text,
            'target_host': sshTarget.text,
            'target_port': targetPort,
          },
      };
  await showDialog(
    context: context,
    builder: (context) => StatefulBuilder(
      builder: (context, setDialog) => AlertDialog(
        title: Text(oldId == null ? '新建连接' : '编辑连接'),
        content: SizedBox(
          width: 460,
          child: Column(mainAxisSize: MainAxisSize.min, children: [
            TextField(
                controller: name,
                decoration: const InputDecoration(labelText: '名称')),
            const SizedBox(height: 8),
            DropdownButtonFormField<String>(
              initialValue: driver,
              items: const [
                DropdownMenuItem(value: 'sqlite', child: Text('SQLite')),
                DropdownMenuItem(value: 'mysql', child: Text('MySQL')),
              ],
              onChanged: (v) => setDialog(() => driver = v!),
            ),
            const SizedBox(height: 8),
            TextField(
                controller: dsn,
                decoration: const InputDecoration(
                    labelText: 'DSN（sqlite 填文件路径，mysql 填连接串）')),
            CheckboxListTile(
              dense: true,
              contentPadding: EdgeInsets.zero,
              title: const Text('经 SSH 隧道', style: TextStyle(fontSize: 13)),
              value: useSsh,
              onChanged: (v) => setDialog(() => useSsh = v!),
            ),
            if (useSsh) ...[
              TextField(
                  controller: sshHost,
                  decoration: const InputDecoration(
                      isDense: true, labelText: '跳板机主机')),
              Row(children: [
                Expanded(
                    child: TextField(
                        controller: sshUser,
                        decoration: const InputDecoration(
                            isDense: true, labelText: '跳板机用户'))),
                const SizedBox(width: 8),
                SizedBox(
                  width: 80,
                  child: TextField(
                      controller: TextEditingController(text: '$sshPort'),
                      decoration: const InputDecoration(
                          isDense: true, labelText: '端口'),
                      keyboardType: TextInputType.number,
                      onChanged: (v) => sshPort = int.tryParse(v) ?? 22)),
              ]),
              TextField(
                  controller: sshKey,
                  decoration: const InputDecoration(
                      isDense: true, labelText: 'SSH 私钥文件路径')),
              Row(children: [
                Expanded(
                    child: TextField(
                        controller: sshTarget,
                        decoration: const InputDecoration(
                            isDense: true, labelText: '目标主机（隧道终点）'))),
                const SizedBox(width: 8),
                SizedBox(
                  width: 80,
                  child: TextField(
                      controller: TextEditingController(text: '$targetPort'),
                      decoration: const InputDecoration(
                          isDense: true, labelText: '目标端口'),
                      keyboardType: TextInputType.number,
                      onChanged: (v) => targetPort = int.tryParse(v) ?? 3306)),
              ]),
            ],
            const SizedBox(height: 8),
            Text(testMsg, style: const TextStyle(fontSize: 12)),
          ]),
        ),
        actions: [
          TextButton(
            onPressed: () async {
              setDialog(() => testMsg = '测试中…');
              try {
                final r = await be.testConnection(cfg());
                setDialog(() =>
                    testMsg = r['ok'] == true ? '连通 ✓' : '不通：${r['error']}');
              } catch (e) {
                setDialog(() => testMsg = '不通：$e');
              }
            },
            child: const Text('测试连通'),
          ),
          TextButton(
              onPressed: () => Navigator.pop(context), child: const Text('取消')),
          FilledButton(
            onPressed: () async {
              try {
                if (oldId != null) {
                  await be.deleteConnection(oldId);
                }
                await be.createConnection(name.text, cfg());
                await onSaved();
                if (context.mounted) Navigator.pop(context);
              } catch (e) {
                setDialog(() => testMsg = '保存失败：$e');
              }
            },
            child: const Text('保存'),
          ),
        ],
      ),
    ),
  );
  sshHost.dispose();
  sshUser.dispose();
  sshKey.dispose();
  sshTarget.dispose();
}
