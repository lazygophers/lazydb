// 结果表格冒烟：万行数据 pump 不炸、可见行数受虚拟滚动限制。
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:lazydb/result_grid.dart';

void main() {
  testWidgets('ResultGrid 万行虚拟滚动', (tester) async {
    final rows =
        List.generate(10000, (i) => <dynamic>[i, 'row-$i', i % 2 == 0 ? null : 'x']);
    await tester.pumpWidget(MaterialApp(
        home: Scaffold(
            body: SizedBox(
                height: 600,
                child: ResultGrid(
                    result: QueryResult(['id', 'name', 'flag'], rows))))));

    // 视口 600px / 行高 26 ≈ 24 行，远小于 10000：虚拟滚动生效
    final texts = tester.widgetList<Text>(find.byType(Text)).toList();
    expect(texts.length, lessThan(100));
    expect(find.text('row-0'), findsOneWidget);
    expect(find.text('row-9999'), findsNothing);

    await tester.drag(find.byType(ListView), const Offset(0, -3000));
    await tester.pumpAndSettle();
    expect(find.text('row-0'), findsNothing);
  });

  test('QueryResult.fromJson 空结果集', () {
    final r = QueryResult.fromJson({'columns': [], 'rows': []});
    expect(r.columns, isEmpty);
    expect(r.rows, isEmpty);
    expect(r.truncated, false);
  });
}
