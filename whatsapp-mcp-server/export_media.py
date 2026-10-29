"""Download the media of a WhatsApp chat and copy it to a folder with a manifest.

Usage: uv run export_media.py <chat_jid> [dest_dir] [--types audio,image] [--after ISO] [--before ISO] [--no-retry]
"""
import argparse
import os
import sys

from whatsapp import export_chat_media


def main() -> int:
    parser = argparse.ArgumentParser(description="Download the media of a WhatsApp chat and write manifest.csv/json")
    parser.add_argument("chat_jid", help="chat JID, e.g. 123456789012345678@g.us")
    parser.add_argument("dest_dir", nargs="?", help="folder to copy the files to (optional)")
    parser.add_argument("--types", help="comma-separated: image,audio,video,document")
    parser.add_argument("--after", help="ISO-8601 date with timezone, e.g. 2026-09-24T00:00:00-03:00")
    parser.add_argument("--before", help="ISO-8601 date with timezone")
    parser.add_argument("--no-retry", action="store_true", help="don't ask the phone to re-upload expired media")
    args = parser.parse_args()

    types = [t.strip() for t in args.types.split(",")] if args.types else None
    dest = os.path.abspath(os.path.expanduser(args.dest_dir)) if args.dest_dir else None
    result = export_chat_media(args.chat_jid, dest, types, args.after, args.before, not args.no_retry)
    print(result.get("message", ""))
    for item in result.get("items", []):
        if item.get("error"):
            print(f"  ERROR {item['message_id']}: {item['error']}")
        elif item.get("file"):
            print(f"  {item.get('status', '')}: {item['file']}")
            if item.get("note"):
                print(f"    WARNING: {item['note']}")
    for problem in result.get("problems", []):
        print(f"  PROBLEM: {problem}")
    if result.get("manifest"):
        print(f"Manifest: {result['manifest']}")
    ok = result.get("success") and not result.get("failed") and not result.get("problems")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
