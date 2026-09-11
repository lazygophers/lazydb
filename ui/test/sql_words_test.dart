// SqlWords 补全逻辑：关键字/表名前缀、`表.` 出字段、字符串内不出、miss 回调。
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:lazydb/sql_editor.dart';
import 'package:re_editor/re_editor.dart';

void main() {
  test('关键字与表名前缀匹配', () {
    final w = SqlWords()..setTables(['orders', 'items']);
    final v = w.buildFor('SELECT * FROM ord', 17);
    expect(v, isNotNull);
    final words = v!.prompts.map((p) => p.word).toList();
    expect(words, contains('orders'));
    expect(words.any((x) => x.startsWith('SEL')), isFalse); // 已整词输入不出自身
    expect(v.input, 'ord');
  });

  test('表名后 . 出字段', () {
    final w = SqlWords()..setColumns('orders', [
      {'name': 'customer', 'type': 'text'},
      {'name': 'cust_id', 'type': 'int'},
    ]);
    final v = w.buildFor('SELECT customer FROM orders.', 28);
    expect(v, isNotNull);
    final words = v!.prompts.map((p) => p.word).toList();
    expect(words, containsAll(['customer', 'cust_id']));
    expect(v.prompts.first, isA<CodeFieldPrompt>());
  });

  test('未加载的表触发 miss 回调', () {
    String? missed;
    final w = SqlWords()..onTableMiss = (t) => missed = t;
    final v = w.buildFor('orders.', 7);
    expect(missed, 'orders');
    expect(v, isNull); // 还没供词，本轮无提示
  });

  test('光标在字符串内不出提示', () {
    final w = SqlWords()..setTables(['orders']);
    expect(w.buildFor("SELECT 'ord'", 11), isNull);
  });

  test('空输入与非词前缀不出提示', () {
    final w = SqlWords()..setTables(['orders']);
    expect(w.buildFor('SELECT ', 7), isNull);
    expect(w.buildFor('order', 5), isNotNull); // 词中出提示
    expect(w.buildFor('orders', 6), isNull); // 整词输完不再提示自身
  });

  testWidgets('万行文本编辑器构建不卡', (tester) async {
    final text = List.generate(
            10000, (i) => 'SELECT col_$i FROM t WHERE x = $i AND s = \'v$i\';')
        .join('\n');
    final c = CodeLineEditingController.fromText(text);
    await tester.pumpWidget(MaterialApp(
        home: Scaffold(body: SqlEditor(controller: c, words: SqlWords()))));
    await tester.pumpAndSettle(const Duration(seconds: 5));
    expect(find.byType(CodeEditor), findsOneWidget);
  });
}
