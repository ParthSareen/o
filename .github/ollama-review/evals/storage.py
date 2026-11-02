RECORDS = {"invoice-1": {"owner": "alice", "value": "SYNTHETIC_INVOICE"}}


def get_record(record_id):
    return RECORDS[record_id]


def require_owner(actor, record):
    if record["owner"] != actor:
        raise PermissionError("record belongs to another user")


def get_record_for_actor(actor, record_id):
    record = get_record(record_id)
    require_owner(actor, record)
    return record
