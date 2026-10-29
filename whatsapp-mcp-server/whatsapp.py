import sqlite3
from datetime import datetime, timezone
from dataclasses import dataclass
from typing import Any, Dict, Optional, List, Tuple
import csv
import hashlib
import os.path
import shutil
import subprocess
import sys
import tempfile
import time
import requests
import json
import audio

try:
    import fcntl
except ImportError:  # Windows: exports into one folder are not serialized
    fcntl = None

MESSAGES_DB_PATH = os.path.join(os.path.dirname(os.path.abspath(__file__)), '..', 'whatsapp-bridge', 'store', 'messages.db')
WHATSMEOW_DB_PATH = os.path.join(os.path.dirname(os.path.abspath(__file__)), '..', 'whatsapp-bridge', 'store', 'whatsapp.db')
WHATSAPP_API_BASE_URL = f"http://127.0.0.1:{os.environ.get('WHATSAPP_BRIDGE_PORT', '8741')}/api"

@dataclass
class Message:
    timestamp: datetime
    sender: str
    content: str
    is_from_me: bool
    chat_jid: str
    id: str
    chat_name: Optional[str] = None
    media_type: Optional[str] = None

@dataclass
class Chat:
    jid: str
    name: Optional[str]
    last_message_time: Optional[datetime]
    last_message: Optional[str] = None
    last_sender: Optional[str] = None
    last_is_from_me: Optional[bool] = None

    @property
    def is_group(self) -> bool:
        """Determine if chat is a group based on JID pattern."""
        return self.jid.endswith("@g.us")

@dataclass
class Contact:
    phone_number: str
    name: Optional[str]
    jid: str

@dataclass
class MessageContext:
    message: Message
    before: List[Message]
    after: List[Message]

def get_sender_name(sender_jid: str) -> str:
    try:
        conn = sqlite3.connect(MESSAGES_DB_PATH)
        cursor = conn.cursor()
        
        # First try matching by exact JID
        cursor.execute("""
            SELECT name
            FROM chats
            WHERE jid = ?
            LIMIT 1
        """, (sender_jid,))
        
        result = cursor.fetchone()
        
        # If no result, try looking for the number within JIDs
        if not result:
            # Extract the phone number part if it's a JID
            if '@' in sender_jid:
                phone_part = sender_jid.split('@')[0]
            else:
                phone_part = sender_jid
                
            cursor.execute("""
                SELECT name
                FROM chats
                WHERE jid LIKE ?
                LIMIT 1
            """, (f"%{phone_part}%",))
            
            result = cursor.fetchone()
        
        if result and result[0]:
            return result[0]
        return contact_name(sender_jid) or sender_jid
        
    except sqlite3.Error as e:
        print(f"Database error while getting sender name: {e}")
        return sender_jid
    finally:
        if 'conn' in locals():
            conn.close()

def format_message(message: Message, show_chat_info: bool = True) -> None:
    """Print a single message with consistent formatting."""
    output = ""
    
    if show_chat_info and message.chat_name:
        output += f"[{message.timestamp:%Y-%m-%d %H:%M:%S}] Chat: {message.chat_name} "
    else:
        output += f"[{message.timestamp:%Y-%m-%d %H:%M:%S}] "
        
    content_prefix = ""
    if hasattr(message, 'media_type') and message.media_type:
        content_prefix = f"[{message.media_type} - Message ID: {message.id} - Chat JID: {message.chat_jid}] "
    
    try:
        sender_name = get_sender_name(message.sender) if not message.is_from_me else "Me"
        output += f"From: {sender_name}: {content_prefix}{message.content}\n"
    except Exception as e:
        print(f"Error formatting message: {e}")
    return output

def format_messages_list(messages: List[Message], show_chat_info: bool = True) -> None:
    output = ""
    if not messages:
        output += "No messages to display."
        return output
    
    for message in messages:
        output += format_message(message, show_chat_info)
    return output

def list_messages(
    after: Optional[str] = None,
    before: Optional[str] = None,
    sender_phone_number: Optional[str] = None,
    chat_jid: Optional[str] = None,
    query: Optional[str] = None,
    limit: int = 20,
    page: int = 0,
    include_context: bool = True,
    context_before: int = 1,
    context_after: int = 1
) -> List[Message]:
    """Get messages matching the specified criteria with optional context."""
    try:
        conn = sqlite3.connect(MESSAGES_DB_PATH)
        cursor = conn.cursor()
        
        # Build base query
        query_parts = ["SELECT messages.timestamp, messages.sender, chats.name, messages.content, messages.is_from_me, chats.jid, messages.id, messages.media_type FROM messages"]
        query_parts.append("JOIN chats ON messages.chat_jid = chats.jid")
        where_clauses = []
        params = []
        
        # Add filters
        if after:
            try:
                after = datetime.fromisoformat(after)
            except ValueError:
                raise ValueError(f"Invalid date format for 'after': {after}. Please use ISO-8601 format.")
            
            where_clauses.append("messages.timestamp > ?")
            params.append(after)

        if before:
            try:
                before = datetime.fromisoformat(before)
            except ValueError:
                raise ValueError(f"Invalid date format for 'before': {before}. Please use ISO-8601 format.")
            
            where_clauses.append("messages.timestamp < ?")
            params.append(before)

        if sender_phone_number:
            where_clauses.append("messages.sender = ?")
            params.append(sender_phone_number)
            
        if chat_jid:
            where_clauses.append("messages.chat_jid = ?")
            params.append(chat_jid)
            
        if query:
            where_clauses.append("LOWER(messages.content) LIKE LOWER(?)")
            params.append(f"%{query}%")
            
        if where_clauses:
            query_parts.append("WHERE " + " AND ".join(where_clauses))
            
        # Add pagination
        offset = page * limit
        query_parts.append("ORDER BY messages.timestamp DESC")
        query_parts.append("LIMIT ? OFFSET ?")
        params.extend([limit, offset])
        
        cursor.execute(" ".join(query_parts), tuple(params))
        messages = cursor.fetchall()
        
        result = []
        for msg in messages:
            message = Message(
                timestamp=datetime.fromisoformat(msg[0]),
                sender=msg[1],
                chat_name=msg[2],
                content=msg[3],
                is_from_me=msg[4],
                chat_jid=msg[5],
                id=msg[6],
                media_type=msg[7]
            )
            result.append(message)
            
        if include_context and result:
            # Add context for each message
            messages_with_context = []
            for msg in result:
                context = get_message_context(msg.id, context_before, context_after)
                messages_with_context.extend(context.before)
                messages_with_context.append(context.message)
                messages_with_context.extend(context.after)
            
            return format_messages_list(messages_with_context, show_chat_info=True)
            
        # Format and display messages without context
        return format_messages_list(result, show_chat_info=True)    
        
    except sqlite3.Error as e:
        print(f"Database error: {e}")
        return []
    finally:
        if 'conn' in locals():
            conn.close()


def get_message_context(
    message_id: str,
    before: int = 5,
    after: int = 5
) -> MessageContext:
    """Get context around a specific message."""
    try:
        conn = sqlite3.connect(MESSAGES_DB_PATH)
        cursor = conn.cursor()
        
        # Get the target message first
        cursor.execute("""
            SELECT messages.timestamp, messages.sender, chats.name, messages.content, messages.is_from_me, chats.jid, messages.id, messages.chat_jid, messages.media_type
            FROM messages
            JOIN chats ON messages.chat_jid = chats.jid
            WHERE messages.id = ?
        """, (message_id,))
        msg_data = cursor.fetchone()
        
        if not msg_data:
            raise ValueError(f"Message with ID {message_id} not found")
            
        target_message = Message(
            timestamp=datetime.fromisoformat(msg_data[0]),
            sender=msg_data[1],
            chat_name=msg_data[2],
            content=msg_data[3],
            is_from_me=msg_data[4],
            chat_jid=msg_data[5],
            id=msg_data[6],
            media_type=msg_data[8]
        )
        
        # Get messages before
        cursor.execute("""
            SELECT messages.timestamp, messages.sender, chats.name, messages.content, messages.is_from_me, chats.jid, messages.id, messages.media_type
            FROM messages
            JOIN chats ON messages.chat_jid = chats.jid
            WHERE messages.chat_jid = ? AND messages.timestamp < ?
            ORDER BY messages.timestamp DESC
            LIMIT ?
        """, (msg_data[7], msg_data[0], before))
        
        before_messages = []
        for msg in cursor.fetchall():
            before_messages.append(Message(
                timestamp=datetime.fromisoformat(msg[0]),
                sender=msg[1],
                chat_name=msg[2],
                content=msg[3],
                is_from_me=msg[4],
                chat_jid=msg[5],
                id=msg[6],
                media_type=msg[7]
            ))
        
        # Get messages after
        cursor.execute("""
            SELECT messages.timestamp, messages.sender, chats.name, messages.content, messages.is_from_me, chats.jid, messages.id, messages.media_type
            FROM messages
            JOIN chats ON messages.chat_jid = chats.jid
            WHERE messages.chat_jid = ? AND messages.timestamp > ?
            ORDER BY messages.timestamp ASC
            LIMIT ?
        """, (msg_data[7], msg_data[0], after))
        
        after_messages = []
        for msg in cursor.fetchall():
            after_messages.append(Message(
                timestamp=datetime.fromisoformat(msg[0]),
                sender=msg[1],
                chat_name=msg[2],
                content=msg[3],
                is_from_me=msg[4],
                chat_jid=msg[5],
                id=msg[6],
                media_type=msg[7]
            ))
        
        return MessageContext(
            message=target_message,
            before=before_messages,
            after=after_messages
        )
        
    except sqlite3.Error as e:
        print(f"Database error: {e}")
        raise
    finally:
        if 'conn' in locals():
            conn.close()


def list_chats(
    query: Optional[str] = None,
    limit: int = 20,
    page: int = 0,
    include_last_message: bool = True,
    sort_by: str = "last_active"
) -> List[Chat]:
    """Get chats matching the specified criteria."""
    try:
        conn = sqlite3.connect(MESSAGES_DB_PATH)
        cursor = conn.cursor()
        
        # Build base query
        query_parts = ["""
            SELECT 
                chats.jid,
                chats.name,
                chats.last_message_time,
                messages.content as last_message,
                messages.sender as last_sender,
                messages.is_from_me as last_is_from_me
            FROM chats
        """]
        
        if include_last_message:
            query_parts.append("""
                LEFT JOIN messages ON chats.jid = messages.chat_jid 
                AND chats.last_message_time = messages.timestamp
            """)
            
        where_clauses = []
        params = []
        
        if query:
            where_clauses.append("(LOWER(chats.name) LIKE LOWER(?) OR chats.jid LIKE ?)")
            params.extend([f"%{query}%", f"%{query}%"])
            
        if where_clauses:
            query_parts.append("WHERE " + " AND ".join(where_clauses))
            
        # Add sorting
        order_by = "chats.last_message_time DESC" if sort_by == "last_active" else "chats.name"
        query_parts.append(f"ORDER BY {order_by}")
        
        # Add pagination
        offset = (page ) * limit
        query_parts.append("LIMIT ? OFFSET ?")
        params.extend([limit, offset])
        
        cursor.execute(" ".join(query_parts), tuple(params))
        chats = cursor.fetchall()
        
        result = []
        for chat_data in chats:
            chat = Chat(
                jid=chat_data[0],
                name=chat_data[1],
                last_message_time=datetime.fromisoformat(chat_data[2]) if chat_data[2] else None,
                last_message=chat_data[3],
                last_sender=chat_data[4],
                last_is_from_me=chat_data[5]
            )
            result.append(chat)
            
        return result
        
    except sqlite3.Error as e:
        print(f"Database error: {e}")
        return []
    finally:
        if 'conn' in locals():
            conn.close()


def search_contacts(query: str) -> List[Contact]:
    """Search contacts by name or phone number."""
    try:
        conn = sqlite3.connect(MESSAGES_DB_PATH)
        cursor = conn.cursor()
        
        # Split query into characters to support partial matching
        search_pattern = '%' +query + '%'
        
        cursor.execute("""
            SELECT DISTINCT 
                jid,
                name
            FROM chats
            WHERE 
                (LOWER(name) LIKE LOWER(?) OR LOWER(jid) LIKE LOWER(?))
                AND jid NOT LIKE '%@g.us'
            ORDER BY name, jid
            LIMIT 50
        """, (search_pattern, search_pattern))
        
        contacts = cursor.fetchall()
        
        result = []
        for contact_data in contacts:
            contact = Contact(
                phone_number=contact_data[0].split('@')[0],
                name=contact_data[1],
                jid=contact_data[0]
            )
            result.append(contact)
            
        return result
        
    except sqlite3.Error as e:
        print(f"Database error: {e}")
        return []
    finally:
        if 'conn' in locals():
            conn.close()


def get_contact_chats(jid: str, limit: int = 20, page: int = 0) -> List[Chat]:
    """Get all chats involving the contact.
    
    Args:
        jid: The contact's JID to search for
        limit: Maximum number of chats to return (default 20)
        page: Page number for pagination (default 0)
    """
    try:
        conn = sqlite3.connect(MESSAGES_DB_PATH)
        cursor = conn.cursor()
        
        cursor.execute("""
            SELECT DISTINCT
                c.jid,
                c.name,
                c.last_message_time,
                m.content as last_message,
                m.sender as last_sender,
                m.is_from_me as last_is_from_me
            FROM chats c
            JOIN messages m ON c.jid = m.chat_jid
            WHERE m.sender = ? OR c.jid = ?
            ORDER BY c.last_message_time DESC
            LIMIT ? OFFSET ?
        """, (jid, jid, limit, page * limit))
        
        chats = cursor.fetchall()
        
        result = []
        for chat_data in chats:
            chat = Chat(
                jid=chat_data[0],
                name=chat_data[1],
                last_message_time=datetime.fromisoformat(chat_data[2]) if chat_data[2] else None,
                last_message=chat_data[3],
                last_sender=chat_data[4],
                last_is_from_me=chat_data[5]
            )
            result.append(chat)
            
        return result
        
    except sqlite3.Error as e:
        print(f"Database error: {e}")
        return []
    finally:
        if 'conn' in locals():
            conn.close()


def get_last_interaction(jid: str) -> str:
    """Get most recent message involving the contact."""
    try:
        conn = sqlite3.connect(MESSAGES_DB_PATH)
        cursor = conn.cursor()
        
        cursor.execute("""
            SELECT 
                m.timestamp,
                m.sender,
                c.name,
                m.content,
                m.is_from_me,
                c.jid,
                m.id,
                m.media_type
            FROM messages m
            JOIN chats c ON m.chat_jid = c.jid
            WHERE m.sender = ? OR c.jid = ?
            ORDER BY m.timestamp DESC
            LIMIT 1
        """, (jid, jid))
        
        msg_data = cursor.fetchone()
        
        if not msg_data:
            return None
            
        message = Message(
            timestamp=datetime.fromisoformat(msg_data[0]),
            sender=msg_data[1],
            chat_name=msg_data[2],
            content=msg_data[3],
            is_from_me=msg_data[4],
            chat_jid=msg_data[5],
            id=msg_data[6],
            media_type=msg_data[7]
        )
        
        return format_message(message)
        
    except sqlite3.Error as e:
        print(f"Database error: {e}")
        return None
    finally:
        if 'conn' in locals():
            conn.close()


def get_chat(chat_jid: str, include_last_message: bool = True) -> Optional[Chat]:
    """Get chat metadata by JID."""
    try:
        conn = sqlite3.connect(MESSAGES_DB_PATH)
        cursor = conn.cursor()
        
        query = """
            SELECT 
                c.jid,
                c.name,
                c.last_message_time,
                m.content as last_message,
                m.sender as last_sender,
                m.is_from_me as last_is_from_me
            FROM chats c
        """
        
        if include_last_message:
            query += """
                LEFT JOIN messages m ON c.jid = m.chat_jid 
                AND c.last_message_time = m.timestamp
            """
            
        query += " WHERE c.jid = ?"
        
        cursor.execute(query, (chat_jid,))
        chat_data = cursor.fetchone()
        
        if not chat_data:
            return None
            
        return Chat(
            jid=chat_data[0],
            name=chat_data[1],
            last_message_time=datetime.fromisoformat(chat_data[2]) if chat_data[2] else None,
            last_message=chat_data[3],
            last_sender=chat_data[4],
            last_is_from_me=chat_data[5]
        )
        
    except sqlite3.Error as e:
        print(f"Database error: {e}")
        return None
    finally:
        if 'conn' in locals():
            conn.close()


def get_direct_chat_by_contact(sender_phone_number: str) -> Optional[Chat]:
    """Get chat metadata by sender phone number."""
    try:
        conn = sqlite3.connect(MESSAGES_DB_PATH)
        cursor = conn.cursor()
        
        cursor.execute("""
            SELECT 
                c.jid,
                c.name,
                c.last_message_time,
                m.content as last_message,
                m.sender as last_sender,
                m.is_from_me as last_is_from_me
            FROM chats c
            LEFT JOIN messages m ON c.jid = m.chat_jid 
                AND c.last_message_time = m.timestamp
            WHERE c.jid LIKE ? AND c.jid NOT LIKE '%@g.us'
            LIMIT 1
        """, (f"%{sender_phone_number}%",))
        
        chat_data = cursor.fetchone()
        
        if not chat_data:
            return None
            
        return Chat(
            jid=chat_data[0],
            name=chat_data[1],
            last_message_time=datetime.fromisoformat(chat_data[2]) if chat_data[2] else None,
            last_message=chat_data[3],
            last_sender=chat_data[4],
            last_is_from_me=chat_data[5]
        )
        
    except sqlite3.Error as e:
        print(f"Database error: {e}")
        return None
    finally:
        if 'conn' in locals():
            conn.close()

def send_message(recipient: str, message: str) -> Tuple[bool, str]:
    try:
        # Validate input
        if not recipient:
            return False, "Recipient must be provided"
        
        url = f"{WHATSAPP_API_BASE_URL}/send"
        payload = {
            "recipient": recipient,
            "message": message,
        }
        
        response = requests.post(url, json=payload)
        
        # Check if the request was successful
        if response.status_code == 200:
            result = response.json()
            return result.get("success", False), result.get("message", "Unknown response")
        else:
            return False, f"Error: HTTP {response.status_code} - {response.text}"
            
    except requests.RequestException as e:
        return False, f"Request error: {str(e)}"
    except json.JSONDecodeError:
        return False, f"Error parsing response: {response.text}"
    except Exception as e:
        return False, f"Unexpected error: {str(e)}"

def send_file(recipient: str, media_path: str) -> Tuple[bool, str]:
    try:
        # Validate input
        if not recipient:
            return False, "Recipient must be provided"
        
        if not media_path:
            return False, "Media path must be provided"
        
        if not os.path.isfile(media_path):
            return False, f"Media file not found: {media_path}"
        
        url = f"{WHATSAPP_API_BASE_URL}/send"
        payload = {
            "recipient": recipient,
            "media_path": media_path
        }
        
        response = requests.post(url, json=payload)
        
        # Check if the request was successful
        if response.status_code == 200:
            result = response.json()
            return result.get("success", False), result.get("message", "Unknown response")
        else:
            return False, f"Error: HTTP {response.status_code} - {response.text}"
            
    except requests.RequestException as e:
        return False, f"Request error: {str(e)}"
    except json.JSONDecodeError:
        return False, f"Error parsing response: {response.text}"
    except Exception as e:
        return False, f"Unexpected error: {str(e)}"

def send_audio_message(recipient: str, media_path: str) -> Tuple[bool, str]:
    try:
        # Validate input
        if not recipient:
            return False, "Recipient must be provided"
        
        if not media_path:
            return False, "Media path must be provided"
        
        if not os.path.isfile(media_path):
            return False, f"Media file not found: {media_path}"

        if not media_path.endswith(".ogg"):
            try:
                media_path = audio.convert_to_opus_ogg_temp(media_path)
            except Exception as e:
                return False, f"Error converting file to opus ogg. You likely need to install ffmpeg: {str(e)}"
        
        url = f"{WHATSAPP_API_BASE_URL}/send"
        payload = {
            "recipient": recipient,
            "media_path": media_path
        }
        
        response = requests.post(url, json=payload)
        
        # Check if the request was successful
        if response.status_code == 200:
            result = response.json()
            return result.get("success", False), result.get("message", "Unknown response")
        else:
            return False, f"Error: HTTP {response.status_code} - {response.text}"
            
    except requests.RequestException as e:
        return False, f"Request error: {str(e)}"
    except json.JSONDecodeError:
        return False, f"Error parsing response: {response.text}"
    except Exception as e:
        return False, f"Unexpected error: {str(e)}"

def _bridge_error(response: requests.Response) -> str:
    try:
        return response.json().get("message") or f"HTTP {response.status_code}"
    except ValueError:
        return f"HTTP {response.status_code}: {response.text.strip()[:200]}"


def download_media(message_id: str, chat_jid: str) -> Tuple[Optional[str], str]:
    """Download media from a message.

    Args:
        message_id: The ID of the message containing the media
        chat_jid: The JID of the chat containing the message

    Returns:
        (local file path or None, status message from the bridge)
    """
    try:
        # A phone re-upload plus a large file can take minutes; the bridge finishes the download
        # even if this gives up, and the next call returns the file from its store
        response = requests.post(
            f"{WHATSAPP_API_BASE_URL}/download",
            json={"message_id": message_id, "chat_jid": chat_jid},
            timeout=(5, 1900),
        )
    except requests.RequestException as e:
        return None, f"Request error: {e}"
    if response.status_code != 200:
        return None, _bridge_error(response)
    try:
        result = response.json()
    except ValueError:
        return None, f"Unexpected response from the bridge: {response.text[:200]}"
    if result.get("success", False):
        return result.get("path"), result.get("message", "Media downloaded successfully")
    return None, result.get("message", "Unknown error")


def request_chat_backfill(chat_jid: str, count: int = 100) -> Tuple[bool, str]:
    """Ask the phone to resend a chat's recent history so the bridge can fill captions,
    sender JIDs and download paths of messages stored by older versions."""
    try:
        response = requests.post(f"{WHATSAPP_API_BASE_URL}/backfill", json={"chat_jid": chat_jid, "count": count}, timeout=(5, 60))
    except requests.RequestException as e:
        return False, f"Request error: {e}"
    if response.status_code != 200:
        return False, _bridge_error(response)
    result = response.json()
    return bool(result.get("success")), result.get("message", "")


def _sha256(path: str) -> str:
    digest = hashlib.sha256()
    with open(path, "rb") as f:
        for block in iter(lambda: f.read(1 << 20), b""):
            digest.update(block)
    return digest.hexdigest()


def contact_name(user_or_jid: str) -> Optional[str]:
    """Name of a phone number or LID from the bridge's contact store (whatsapp.db)."""
    user = user_or_jid.split("@")[0].split(":")[0]
    if not user:
        return None
    try:
        conn = sqlite3.connect(f"file:{WHATSMEOW_DB_PATH}?mode=ro", uri=True)
        try:
            cursor = conn.cursor()
            users = {user}
            cursor.execute("SELECT pn FROM whatsmeow_lid_map WHERE lid = ? UNION SELECT lid FROM whatsmeow_lid_map WHERE pn = ?", (user, user))
            users.update(row[0] for row in cursor.fetchall() if row[0])
            jids = [f"{u}@{server}" for u in users for server in ("s.whatsapp.net", "lid")]
            cursor.execute(
                f"SELECT full_name, push_name, business_name FROM whatsmeow_contacts WHERE their_jid IN ({','.join('?' * len(jids))})",
                jids,
            )
            for full_name, push_name, business_name in cursor.fetchall():
                for name in (full_name, push_name, business_name):
                    if name:
                        return name
        finally:
            conn.close()
    except sqlite3.Error:
        return None
    return None


def _media_duration(path: str) -> Any:
    """Duration in whole seconds read with ffprobe, or "" when it isn't available."""
    ffprobe = shutil.which("ffprobe") or "/opt/homebrew/bin/ffprobe"
    try:
        out = subprocess.run(
            [ffprobe, "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", path],
            capture_output=True, text=True, timeout=30,
        ).stdout.strip()
        return round(float(out)) if out else ""
    except (OSError, ValueError, subprocess.SubprocessError):
        return ""


MANIFEST_FIELDS = [
    "chat_jid", "chat_name", "message_id", "timestamp", "sender", "sender_id", "from_me",
    "media_type", "mime_type", "duration_s", "forwarded", "caption", "original_name", "file",
    "size", "sha256", "media_retry", "exported_at", "status", "note", "replaced_at", "moved_aside",
]

# Shown in the CSV when the bridge that stored the message did not record the caption yet
CAPTION_UNKNOWN = "(not recorded)"


def _full_jid(sender_id: str) -> str:
    """Give a bare user part its server when the contact store says which one it is.
    Unknown values stay bare rather than being guessed into a phone number."""
    if not sender_id or "@" in sender_id:
        return sender_id
    try:
        conn = sqlite3.connect(f"file:{WHATSMEOW_DB_PATH}?mode=ro", uri=True)
        try:
            if conn.execute("SELECT 1 FROM whatsmeow_lid_map WHERE lid = ?", (sender_id,)).fetchone():
                return f"{sender_id}@lid"
            if conn.execute("SELECT 1 FROM whatsmeow_lid_map WHERE pn = ?", (sender_id,)).fetchone() or conn.execute(
                "SELECT 1 FROM whatsmeow_contacts WHERE their_jid = ?", (f"{sender_id}@s.whatsapp.net",)
            ).fetchone():
                return f"{sender_id}@s.whatsapp.net"
        finally:
            conn.close()
    except sqlite3.Error:
        pass
    return sender_id


def _csv_safe(value: Any) -> Any:
    """Keep spreadsheets from reading text cells as formulas."""
    if isinstance(value, str) and value[:1] in ("=", "+", "-", "@", "\t", "\r"):
        return "'" + value
    return value


def _write_atomic(path: str, write) -> None:
    fd, tmp = tempfile.mkstemp(dir=os.path.dirname(path), prefix=f".{os.path.basename(path)}.", suffix=".tmp")
    try:
        with os.fdopen(fd, "w", encoding="utf-8-sig" if path.endswith(".csv") else "utf-8", newline="") as f:
            write(f)
            f.flush()
            os.fsync(f.fileno())
        os.chmod(tmp, 0o644)
        os.replace(tmp, path)
    except BaseException:
        if os.path.exists(tmp):
            os.remove(tmp)
        raise


def _copy_verified(src: str, dst: str, sha256: str) -> bool:
    """Copy through a temporary name so an interrupted copy never sits under the final name."""
    part = f"{dst}.part"
    try:
        shutil.copy2(src, part)
        if _sha256(part) != sha256:
            os.remove(part)
            return False
        os.chmod(part, 0o444)
        os.replace(part, dst)
        return True
    except BaseException:
        if os.path.exists(part):
            os.remove(part)
        raise


def _move_aside(path: str) -> Dict[str, str]:
    aside = f"{path}.mismatch-{int(time.time())}"
    digest = _sha256(path)
    os.rename(path, aside)
    return {"file": os.path.basename(aside), "sha256": digest}


def _load_manifest(path: str, problems: List[str]) -> Dict[Tuple[str, str], Dict[str, Any]]:
    """Entries keyed by (chat_jid, message_id). Entries written by the first version get their
    chat from the header and the current meaning of caption/forwarded/status."""
    entries: Dict[Tuple[str, str], Dict[str, Any]] = {}
    if not os.path.exists(path):
        return entries
    try:
        with open(path, encoding="utf-8") as f:
            data = json.load(f)
    except ValueError:
        aside = f"{path}.corrupt-{int(time.time())}"
        os.rename(path, aside)
        problems.append(f"manifest.json could not be read and was moved to {os.path.basename(aside)}; it lists only this run's files now")
        return entries
    for entry in data.get("files", []):
        if data.get("chat_jid"):
            entry.setdefault("chat_jid", data["chat_jid"])
            entry.setdefault("chat_name", data.get("chat_name", data["chat_jid"]))
        if "error" in entry:
            error = entry.pop("error")
            if error and not entry.get("status"):
                entry.update({"status": "failed", "note": error})
        if entry.get("forwarded") == "":
            entry["forwarded"] = None
            if entry.get("caption") == "" and entry.get("media_type") != "audio":
                entry["caption"] = None
        if entry.get("sender_id"):
            entry["sender_id"] = _full_jid(entry["sender_id"])
        entries[(entry.get("chat_jid", ""), entry["message_id"])] = entry
    return entries


def export_chat_media(
    chat_jid: str,
    dest_dir: Optional[str] = None,
    media_types: Optional[List[str]] = None,
    after: Optional[str] = None,
    before: Optional[str] = None,
    media_retry: bool = True,
) -> Dict[str, Any]:
    """Download every media file of a chat through the bridge and, if dest_dir is given,
    copy the files there (read-only) with a manifest.csv/manifest.json holding the chat, date,
    sender, caption and SHA-256 of each message. Files already exported are checked, not rewritten;
    files that went missing or changed are flagged in the manifest and in "problems".
    dest_dir must be absolute (or start with ~): the server does not run in the caller's directory.
    """
    if dest_dir:
        dest_dir = os.path.expanduser(dest_dir)
        if not os.path.isabs(dest_dir):
            return {"success": False, "message": f"dest_dir must be an absolute path, got {dest_dir!r}"}

    payload: Dict[str, Any] = {"chat_jid": chat_jid, "media_retry": media_retry}
    if media_types:
        payload["media_types"] = media_types
    if after:
        payload["after"] = after
    if before:
        payload["before"] = before
    try:
        # The bridge answers once every item was tried; downloads go on in the bridge even if this gives up
        response = requests.post(f"{WHATSAPP_API_BASE_URL}/download_chat", json=payload, timeout=(5, 4 * 3600))
    except requests.RequestException as e:
        return {"success": False, "message": f"Request error: {e}"}
    if response.status_code != 200:
        return {"success": False, "message": _bridge_error(response)}
    result = response.json()
    if not result.get("success"):
        return {"success": False, "message": result.get("message", "Unknown error")}

    summary: Dict[str, Any] = {
        "success": True,
        "message": result.get("message", ""),
        "total": result.get("total", 0),
        "downloaded": result.get("downloaded", 0),
        "cached": result.get("cached", 0),
        "failed": result.get("failed", 0),
        "items": [],
        "problems": [],
    }
    items = result.get("items") or []
    if not dest_dir:
        summary["items"] = [
            {"message_id": it["message_id"], "path": (it.get("media") or {}).get("path"), "error": it.get("error")}
            for it in items
        ]
        return summary

    os.makedirs(dest_dir, exist_ok=True)
    # One export at a time per folder, so two chats never overwrite each other's manifest rows
    with open(os.path.join(dest_dir, ".manifest.lock"), "w") as lock:
        if fcntl:
            fcntl.flock(lock, fcntl.LOCK_EX)
        _export_into(dest_dir, chat_jid, items, summary)

    summary["dest_dir"] = dest_dir
    summary["manifest"] = os.path.join(dest_dir, "manifest.json")
    summary["message"] += f"; {summary['copied']} new files copied to {dest_dir}"
    if summary["problems"]:
        summary["message"] += f"; {len(summary['problems'])} problem(s) found in the folder"
    return summary


def _export_into(dest_dir: str, chat_jid: str, items: List[Dict[str, Any]], summary: Dict[str, Any]) -> None:
    problems: List[str] = summary["problems"]
    manifest_json = os.path.join(dest_dir, "manifest.json")
    manifest = _load_manifest(manifest_json, problems)

    captions: Dict[str, str] = {}
    chat_name = chat_jid
    conn = sqlite3.connect(f"file:{MESSAGES_DB_PATH}?mode=ro", uri=True)
    try:
        ids = [it["message_id"] for it in items]
        if ids:
            cursor = conn.execute(
                f"SELECT id, content FROM messages WHERE chat_jid = ? AND id IN ({','.join('?' * len(ids))})",
                [chat_jid, *ids],
            )
            captions = {row[0]: row[1] or "" for row in cursor.fetchall()}
        row = conn.execute("SELECT name FROM chats WHERE jid = ?", (chat_jid,)).fetchone()
        if row and row[0]:
            chat_name = row[0]
    finally:
        conn.close()

    names: Dict[str, str] = {}
    now = datetime.now(timezone.utc).isoformat(timespec="seconds")
    summary["copied"] = 0
    processed = set()
    try:
        for it in items:
            media = it.get("media") or {}
            message_id = it["message_id"]
            key = (chat_jid, message_id)
            entry = dict(manifest.get(key, {}))
            entry.update({"chat_jid": chat_jid, "chat_name": chat_name, "message_id": message_id})
            if not it.get("success"):
                # Keep a file exported earlier; only record the error when nothing was exported
                if not entry.get("file"):
                    entry.update({"status": "failed", "note": it.get("error", "unknown error")})
                    manifest[key] = entry
                summary["items"].append({"message_id": message_id, "error": it.get("error")})
                continue

            sender_id = _full_jid(media.get("sender_jid") or media.get("sender") or "")
            if sender_id not in names:
                names[sender_id] = "Me" if media.get("is_from_me") else (contact_name(sender_id) or sender_id)

            dst = os.path.join(dest_dir, media["filename"])
            # Copies in the folder that claim to be this message but no longer match it are kept aside
            previous = os.path.join(dest_dir, entry["file"]) if entry.get("file") else None
            match, moved_now = None, []
            for candidate in dict.fromkeys(p for p in (previous, dst) if p):
                if not os.path.exists(candidate):
                    continue
                if _sha256(candidate) == media["sha256"]:
                    match = candidate
                    break
                moved_now.append(_move_aside(candidate))
            if match:
                dst, status = match, "already exported"
            else:
                if not _copy_verified(media["path"], dst, media["sha256"]):
                    entry.update({"status": "failed", "note": "copy does not match the downloaded file"})
                    manifest[key] = entry
                    summary["items"].append({"message_id": message_id, "error": entry["note"]})
                    continue
                summary["copied"] += 1
                status = "replaced" if moved_now else "exported"
            note = ""
            if moved_now:
                note = "copy in the folder did not match the message; moved to " + ", ".join(m["file"] for m in moved_now)
                entry["replaced_at"] = now
                problems.append(f"{message_id}: {note}")
            moved = list(entry.get("moved_aside") or []) + moved_now

            timestamp = datetime.fromisoformat(media["timestamp"].replace("Z", "+00:00")).astimezone()
            stored_before_patch = media.get("is_forwarded") is None
            caption = captions.get(message_id, "")
            entry.update({
                "timestamp": timestamp.isoformat(timespec="seconds"),
                "sender": names[sender_id],
                "sender_id": sender_id,
                "from_me": bool(media.get("is_from_me")),
                "media_type": media.get("media_type", ""),
                "mime_type": media.get("mime_type", ""),
                "duration_s": media.get("duration") or (_media_duration(dst) if media.get("media_type") in ("audio", "video") else ""),
                # null when the bridge that stored the message did not record it
                "forwarded": None if stored_before_patch else bool(media["is_forwarded"]),
                # Only photos, videos and documents carry captions
                "caption": None if stored_before_patch and not caption and media.get("media_type") != "audio" else caption,
                "original_name": media.get("original_name", ""),
                "file": os.path.basename(dst),
                "size": media.get("size", 0),
                "sha256": media["sha256"],
                "media_retry": bool(media.get("media_retry")),
                "status": status,
                "note": note,
                "moved_aside": moved,
            })
            entry.setdefault("exported_at", now)
            manifest[key] = entry
            processed.add(key)
            summary["items"].append({"message_id": message_id, "file": dst, "sha256": media["sha256"], "status": status, "note": note})
    finally:
        # Every listed file must still be in the folder with the recorded hash. Files handled in
        # this run were just checked; the others (earlier runs, other chats) are checked here.
        for key, entry in manifest.items():
            if not entry.get("file") or key in processed:
                continue
            path = os.path.join(dest_dir, entry["file"])
            try:
                if not os.path.exists(path):
                    entry.update({"status": "missing", "note": "file no longer in the folder"})
                elif _sha256(path) != entry.get("sha256"):
                    entry.update({"status": "altered", "note": "file content differs from the recorded SHA-256"})
                elif entry.get("status") in ("missing", "altered", "unreadable"):
                    entry.update({"status": "already exported", "note": ""})
            except OSError as e:
                entry.update({"status": "unreadable", "note": f"could not read the file: {e}"})
            if entry.get("status") in ("missing", "altered", "unreadable"):
                problems.append(f"{entry['message_id']} ({entry['file']}): {entry['note']}")

        files = sorted(manifest.values(), key=lambda e: (e.get("timestamp", ""), e.get("chat_jid", ""), e["message_id"]))
        chats = {e.get("chat_jid", ""): e.get("chat_name", "") for e in files}
        _write_atomic(manifest_json, lambda f: json.dump(
            {"updated_at": now, "chats": chats, "files": files}, f, ensure_ascii=False, indent=2))

        def write_csv(f):
            writer = csv.DictWriter(f, fieldnames=MANIFEST_FIELDS, extrasaction="ignore")
            writer.writeheader()
            for e in files:
                row = {k: _csv_safe(v) for k, v in e.items()}
                if e.get("caption") is None and e.get("file") and e.get("media_type") != "audio":
                    row["caption"] = CAPTION_UNKNOWN
                if e.get("forwarded") is None:
                    row["forwarded"] = ""
                row["moved_aside"] = "; ".join(f"{m['file']} ({m['sha256'][:12]})" for m in e.get("moved_aside") or [])
                writer.writerow(row)
        _write_atomic(os.path.join(dest_dir, "manifest.csv"), write_csv)
