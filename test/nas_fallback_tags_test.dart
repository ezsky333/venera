import 'package:flutter_test/flutter_test.dart';
import 'package:venera/foundation/nas_client.dart';

// NAS 兜底详情页的标签分组还原。
//
// 背景：服务端把标签存成平铺的 "Namespace:Value" 字符串
// （Categories:長篇 / Tags:蘿莉 / Chinese Team:xxx…）。
// 早期实现把它们整体塞进一个叫 "Tag" 的分组，兜底详情页于是显示成
// 一长串「Tag: Categories:長篇 / Tags:蘿莉」，与源站的
// 「作者 / 汉化组 / 分类 / 标签」分组完全对不上。
void main() {
  NasComicFallback fb(List<String> tags, {String author = '', String desc = ''}) =>
      NasComicFallback(
        sourceKey: 'picacg',
        comicId: '638c49da60e4d613c3696b2c',
        title: '真夜中の夜子さん',
        tags: tags,
        author: author,
        description: desc,
        chapters: const [NasFallbackChapter(id: 'c1', title: '第1话', pages: 10)],
      );

  group('nasFallbackToComicDetailsJson', () {
    test('按命名空间切回分组，而不是全塞进 Tag', () {
      final json = nasFallbackToComicDetailsJson(
        fb([
          'Chinese Team:aisakaguya整合',
          'Categories:長篇',
          'Categories:單行本',
          'Tags:蘿莉',
          'Tags:貧乳',
        ]),
        'picacg',
      );
      final tags = json['tags'] as Map<String, List<String>>;
      expect(tags.keys, containsAll(['Chinese Team', 'Categories', 'Tags']));
      expect(tags['Categories'], ['長篇', '單行本']);
      expect(tags['Tags'], ['蘿莉', '貧乳']);
      expect(tags.containsKey('Tag'), isFalse,
          reason: '有命名空间的标签不应再落进通用 Tag 组');
    });

    test('无命名空间的标签归入 Tag 组且不丢数据', () {
      final json = nasFallbackToComicDetailsJson(
        fb(['蘿莉', 'Tags:貧乳', '']),
        'picacg',
      );
      final tags = json['tags'] as Map<String, List<String>>;
      expect(tags['Tag'], ['蘿莉']);
      expect(tags['Tags'], ['貧乳']);
    });

    test('description / uploader 透传', () {
      final json = nasFallbackToComicDetailsJson(
        fb(const [], desc: '更新 原话：我将…', ),
        'picacg',
      );
      expect(json['description'], '更新 原话：我将…');
    });

    test('作者字段补进 Author 标签组', () {
      final json = nasFallbackToComicDetailsJson(
        fb(['Tags:蘿莉'], author: '砂漠'),
        'picacg',
      );
      final tags = json['tags'] as Map<String, List<String>>;
      expect(tags['Author'], ['砂漠']);
    });

    test('作者可从 Author: 标签回落', () {
      final json = nasFallbackToComicDetailsJson(
        fb(['Author:砂漠', 'Tags:蘿莉']),
        'picacg',
      );
      final tags = json['tags'] as Map<String, List<String>>;
      expect(tags['Author'], ['砂漠']);
    });
  });
}
