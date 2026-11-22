#!/usr/bin/env python3
"""从 cache/dbbackup/data-20261030-120013.sql 恢复 board 数据到 workflow 库。
- 只处理 boards / board_lists / board_cards（board_ai_chats 表已不存在，跳过）。
- 按主键 id 跳过当前库已存在的行（INSERT IGNORE，幂等，不覆盖线上数据）。
- error_logs 等追加表不导入（id 与线上重叠）。
用法：python3 restore-board-data.py | docker exec -i workflow-mysql sh -c 'mysql -uroot -p<pwd> workflow'
"""
import ast
import sys

SRC = "/home/wetee/work/cathome/workflow/cache/dbbackup/data-20261030-120013.sql"
COLS = {
    "boards": ["id", "name", "description", "view_mode", "created_at", "updated_at"],
    "board_lists": ["id", "board_id", "name", "position", "created_at"],
    "board_cards": [
        "id", "list_id", "title", "description", "position", "assignee", "labels",
        "due_date", "source", "target_name", "created_at", "updated_at", "priority",
        "cover_color", "checklist", "status", "work_item_no", "depends_on",
        "frequency", "source_ref",
    ],
}


def sqlval(v):
    if v is None:
        return "NULL"
    if isinstance(v, int):
        return str(v)
    s = str(v)
    return "'" + s.replace("\\", "\\\\").replace("'", "\\'") + "'"


def main():
    out = []
    with open(SRC, encoding="utf-8") as f:
        content = f.read()
    for table in ("boards", "board_lists", "board_cards"):
        marker = f"INSERT INTO `{table}` VALUES "
        idx = content.find(marker)
        if idx < 0:
            print(f"# {table}: no INSERT, skip", file=sys.stderr)
            continue
        end = content.find(";\n/*!40000 ALTER TABLE", idx)
        if end < 0:
            end = content.find(";\n\nLOCK TABLES", idx)
        blob = content[idx + len(marker):end]
        rows = ast.literal_eval("[" + blob.rstrip(";").replace("NULL", "None") + "]")
        if not isinstance(rows[0], tuple):
            rows = (rows,)
        cols = COLS[table]
        ncol = len(cols)
        kept = 0
        for r in rows:
            if len(r) != ncol:
                print(f"# {table} row {r[0]}: {len(r)} cols != {ncol}, skip", file=sys.stderr)
                continue
            vals = ",".join(sqlval(x) for x in r)
            collist = ",".join(f"`{c}`" for c in cols)
            out.append(f"INSERT IGNORE INTO `{table}` ({collist}) VALUES ({vals});")
            kept += 1
        print(f"# {table}: {kept}/{len(rows)} rows ready", file=sys.stderr)
    print("\n".join(out))


if __name__ == "__main__":
    main()
