package com.gmcli;

import java.io.InputStream;
import java.io.OutputStream;
import java.lang.reflect.Field;
import java.lang.reflect.Method;
import java.security.MessageDigest;
import java.util.ArrayList;
import java.util.HashMap;
import java.util.List;
import java.util.Map;

/**
 * Read-only Telephony-provider dumper. This deliberately uses reflection so it
 * can be compiled without android.jar; it runs under Android's shell UID via
 * app_process, which has the same provider access as `adb shell content`.
 */
public final class TelephonyExport {
    private static final int FIELD_NULL = 0;
    private static final int FIELD_INTEGER = 1;
    private static final int FIELD_FLOAT = 2;
    private static final int FIELD_STRING = 3;
    private static final int FIELD_BLOB = 4;

    private final OutputStream out = System.out;
    private Object activityManager;
    private Object attributionSource;
    private Class<?> uriClass;
    private Class<?> cursorClass;
    private Class<?> binderClass;
    private Class<?> providerClass;
    private Class<?> bundleClass;
    private Class<?> cancellationSignalClass;
    private Method uriParse;
    private Method uriGetAuthority;
    private Method acquireProvider;
    private Method releaseProvider;
    private Method providerQuery;
    private Method providerOpenFile;
    private Method cursorMoveToNext;
    private Method cursorGetColumnNames;
    private Method cursorGetColumnIndex;
    private Method cursorGetType;
    private Method cursorGetLong;
    private Method cursorGetDouble;
    private Method cursorGetString;
    private Method cursorGetBlob;
    private Method cursorClose;
    private boolean includePartData = true;
    private String deviceSerial = "";
    private long smsCount;
    private long mmsCount;
    private long partCount;
    private long partDataCount;
    private long addressCount;
    private long threadCount;
    private long canonicalAddressCount;
    private final Map<Long, Long> mmsThreads = new HashMap<Long, Long>();

    public static void main(String[] args) {
        try {
            TelephonyExport exporter = new TelephonyExport();
            exporter.parseArgs(args);
            exporter.run();
            // app_process initializes Android binder/runtime threads which may
            // keep the VM alive after main returns. The summary has been
            // flushed at this point, so terminate explicitly.
            System.exit(0);
        } catch (Throwable t) {
            t.printStackTrace(System.err);
            System.exit(2);
        }
    }

    private void parseArgs(String[] args) {
        for (int i = 0; i < args.length; i++) {
            if ("--no-part-data".equals(args[i])) {
                includePartData = false;
            } else if ("--device-serial".equals(args[i]) && i + 1 < args.length) {
                deviceSerial = args[++i];
            } else {
                throw new IllegalArgumentException("unknown argument: " + args[i]);
            }
        }
    }

    private void run() throws Exception {
        initializeAndroid();
        writeMetadata();
        queryRows("sms", "content://sms", null, null);
        List<Long> mmsIDs = queryRows("mms", "content://mms", null, null);
        for (Long id : mmsIDs) {
            queryRows("mms_address", "content://mms/" + id + "/addr", "mms_id", id);
        }
        queryParts();
        queryRows("thread", "content://mms-sms/conversations?simple=true", null, null);
        queryRows("canonical_address", "content://mms-sms/canonical-addresses", null, null);
        writeFooter();
        out.flush();
    }

    private void initializeAndroid() throws Exception {
        uriClass = Class.forName("android.net.Uri");
        cursorClass = Class.forName("android.database.Cursor");
        binderClass = Class.forName("android.os.IBinder");
        providerClass = Class.forName("android.content.IContentProvider");
        bundleClass = Class.forName("android.os.Bundle");
        cancellationSignalClass = Class.forName("android.os.ICancellationSignal");
        uriParse = uriClass.getMethod("parse", String.class);
        uriGetAuthority = uriClass.getMethod("getAuthority");
        Class<?> activityManagerClass = Class.forName("android.app.ActivityManager");
        activityManager = activityManagerClass.getMethod("getService").invoke(null);
        Class<?> activityManagerInterface = Class.forName("android.app.IActivityManager");
        acquireProvider = activityManagerInterface.getMethod("getContentProviderExternal",
                String.class, int.class, binderClass, String.class);
        releaseProvider = activityManagerInterface.getMethod("removeContentProviderExternalAsUser",
                String.class, binderClass, int.class);
        providerQuery = providerClass.getMethod("query", Class.forName("android.content.AttributionSource"),
                uriClass, String[].class, bundleClass, cancellationSignalClass);
        providerOpenFile = providerClass.getMethod("openFile", Class.forName("android.content.AttributionSource"),
                uriClass, String.class, cancellationSignalClass);
        attributionSource = Class.forName("android.content.AttributionSource")
                .getConstructor(int.class, String.class, String.class)
                .newInstance(Integer.valueOf(2000), "com.android.shell", null);
        cursorMoveToNext = cursorClass.getMethod("moveToNext");
        cursorGetColumnNames = cursorClass.getMethod("getColumnNames");
        cursorGetColumnIndex = cursorClass.getMethod("getColumnIndex", String.class);
        cursorGetType = cursorClass.getMethod("getType", int.class);
        cursorGetLong = cursorClass.getMethod("getLong", int.class);
        cursorGetDouble = cursorClass.getMethod("getDouble", int.class);
        cursorGetString = cursorClass.getMethod("getString", int.class);
        cursorGetBlob = cursorClass.getMethod("getBlob", int.class);
        cursorClose = cursorClass.getMethod("close");
    }

    private void writeMetadata() throws Exception {
        writeAscii("{\"record_type\":\"metadata\",\"format\":\"gmcli-android-telephony\",\"format_version\":1,\"exported_at_ms\":");
        writeAscii(Long.toString(System.currentTimeMillis()));
        writeAscii(",\"device_serial\":");
        writeJSONString(deviceSerial);
        writeAscii(",\"android\":{");
        writeAscii("\"fingerprint\":");
        writeJSONString(staticString("android.os.Build", "FINGERPRINT"));
        writeAscii(",\"model\":");
        writeJSONString(staticString("android.os.Build", "MODEL"));
        writeAscii(",\"sdk_int\":");
        writeAscii(Integer.toString(staticInt("android.os.Build$VERSION", "SDK_INT")));
        writeAscii("},\"part_data_included\":");
        writeAscii(includePartData ? "true" : "false");
        writeAscii("}\n");
    }

    private String staticString(String className, String fieldName) throws Exception {
        Field field = Class.forName(className).getField(fieldName);
        Object value = field.get(null);
        return value == null ? "" : value.toString();
    }

    private int staticInt(String className, String fieldName) throws Exception {
        return Class.forName(className).getField(fieldName).getInt(null);
    }

    /** Returns _id values when the queried rows have an _id column. */
    private List<Long> queryRows(String recordType, String uriText, String parentName, Long parentID) throws Exception {
        Object uri = uriParse.invoke(null, uriText);
        ProviderHandle handle = acquire(uri);
        Object cursor = providerQuery.invoke(handle.provider, attributionSource, uri, null,
                bundleClass.newInstance(), null);
        if (cursor == null) {
            handle.close();
            throw new IllegalStateException("provider returned a null cursor for " + uriText);
        }
        List<Long> ids = new ArrayList<Long>();
        try {
            String[] columns = (String[]) cursorGetColumnNames.invoke(cursor);
            int idIndex = ((Integer) cursorGetColumnIndex.invoke(cursor, "_id")).intValue();
            while (((Boolean) cursorMoveToNext.invoke(cursor)).booleanValue()) {
                if (idIndex >= 0 && ((Integer) cursorGetType.invoke(cursor, idIndex)).intValue() == FIELD_INTEGER) {
                    ids.add((Long) cursorGetLong.invoke(cursor, idIndex));
                }
                writeRow(recordType, uriText, parentName, parentID, cursor, columns);
                increment(recordType);
            }
        } finally {
            cursorClose.invoke(cursor);
            handle.close();
        }
        return ids;
    }

    private void increment(String recordType) {
        if ("sms".equals(recordType)) smsCount++;
        else if ("mms".equals(recordType)) mmsCount++;
        else if ("mms_address".equals(recordType)) addressCount++;
        else if ("thread".equals(recordType)) threadCount++;
        else if ("canonical_address".equals(recordType)) canonicalAddressCount++;
    }

    private void writeRow(String recordType, String uriText, String parentName, Long parentID,
                          Object cursor, String[] columns) throws Exception {
        writeAscii("{\"record_type\":");
        writeJSONString(recordType);
        writeAscii(",\"source_uri\":");
        writeJSONString(uriText);
        if (parentName != null) {
            writeAscii(",");
            writeJSONString(parentName);
            writeAscii(":");
            writeAscii(Long.toString(parentID.longValue()));
        }
        writeAscii(",\"values\":{");
        for (int i = 0; i < columns.length; i++) {
            if (i != 0) writeAscii(",");
            writeJSONString(columns[i]);
            writeAscii(":");
            writeTaggedValue(cursor, i);
        }
        writeAscii("}}\n");
    }

    private void writeTaggedValue(Object cursor, int column) throws Exception {
        int type = ((Integer) cursorGetType.invoke(cursor, column)).intValue();
        switch (type) {
            case FIELD_NULL:
                writeAscii("{\"type\":\"null\",\"value\":null}");
                return;
            case FIELD_INTEGER:
                writeAscii("{\"type\":\"integer\",\"value\":");
                writeAscii(Long.toString(((Long) cursorGetLong.invoke(cursor, column)).longValue()));
                writeAscii("}");
                return;
            case FIELD_FLOAT:
                double value = ((Double) cursorGetDouble.invoke(cursor, column)).doubleValue();
                writeAscii("{\"type\":\"float\",\"value\":");
                if (Double.isNaN(value)) writeJSONString("NaN");
                else if (value == Double.POSITIVE_INFINITY) writeJSONString("Infinity");
                else if (value == Double.NEGATIVE_INFINITY) writeJSONString("-Infinity");
                else writeAscii(Double.toString(value));
                writeAscii("}");
                return;
            case FIELD_STRING:
                writeAscii("{\"type\":\"string\",\"value\":");
                writeJSONString((String) cursorGetString.invoke(cursor, column));
                writeAscii("}");
                return;
            case FIELD_BLOB:
                writeAscii("{\"type\":\"blob\",\"encoding\":\"base64\",\"value\":");
                writeJSONString(base64((byte[]) cursorGetBlob.invoke(cursor, column)));
                writeAscii("}");
                return;
            default:
                throw new IllegalStateException("unknown cursor field type " + type);
        }
    }

    private void queryParts() throws Exception {
        String uriText = "content://mms/part";
        Object uri = uriParse.invoke(null, uriText);
        ProviderHandle handle = acquire(uri);
        Object cursor = providerQuery.invoke(handle.provider, attributionSource, uri, null,
                bundleClass.newInstance(), null);
        if (cursor == null) throw new IllegalStateException("provider returned a null cursor for " + uriText);
        try {
            String[] columns = (String[]) cursorGetColumnNames.invoke(cursor);
            int idIndex = ((Integer) cursorGetColumnIndex.invoke(cursor, "_id")).intValue();
            int midIndex = ((Integer) cursorGetColumnIndex.invoke(cursor, "mid")).intValue();
            int dataIndex = ((Integer) cursorGetColumnIndex.invoke(cursor, "_data")).intValue();
            if (idIndex < 0 || midIndex < 0) throw new IllegalStateException("MMS part cursor lacks _id or mid");
            while (((Boolean) cursorMoveToNext.invoke(cursor)).booleanValue()) {
                long partID = ((Long) cursorGetLong.invoke(cursor, idIndex)).longValue();
                long mmsID = ((Long) cursorGetLong.invoke(cursor, midIndex)).longValue();
                writeRow("mms_part", uriText, "mms_id", Long.valueOf(mmsID), cursor, columns);
                partCount++;
                boolean hasData = dataIndex >= 0 && ((Integer) cursorGetType.invoke(cursor, dataIndex)).intValue() != FIELD_NULL;
                if (includePartData && hasData) {
                    writePartData(partID, mmsID);
                    partDataCount++;
                }
            }
        } finally {
            cursorClose.invoke(cursor);
            handle.close();
        }
    }

    private void writePartData(long partID, long mmsID) throws Exception {
        String uriText = "content://mms/part/" + partID;
        Object uri = uriParse.invoke(null, uriText);
        ProviderHandle handle = acquire(uri);
        Object descriptor = providerOpenFile.invoke(handle.provider, attributionSource, uri, "r", null);
        Class<?> descriptorClass = Class.forName("android.os.ParcelFileDescriptor");
        InputStream in = (InputStream) Class.forName("android.os.ParcelFileDescriptor$AutoCloseInputStream")
                .getConstructor(descriptorClass).newInstance(descriptor);
        if (in == null) throw new IllegalStateException("provider returned no data for " + uriText);
        MessageDigest digest = MessageDigest.getInstance("SHA-256");
        long size = 0;
        writeAscii("{\"record_type\":\"mms_part_data\",\"source_uri\":");
        writeJSONString(uriText);
        writeAscii(",\"part_id\":");
        writeAscii(Long.toString(partID));
        writeAscii(",\"mms_id\":");
        writeAscii(Long.toString(mmsID));
        writeAscii(",\"encoding\":\"base64\",\"data\":\"");
        Base64Stream encoder = new Base64Stream(out);
        try {
            byte[] buffer = new byte[65536];
            int n;
            while ((n = in.read(buffer)) != -1) {
                digest.update(buffer, 0, n);
                encoder.write(buffer, 0, n);
                size += n;
            }
            encoder.finish();
        } finally {
            in.close();
            handle.close();
        }
        writeAscii("\",\"byte_length\":");
        writeAscii(Long.toString(size));
        writeAscii(",\"sha256\":");
        writeJSONString(hex(digest.digest()));
        writeAscii("}\n");
    }

    private void writeFooter() throws Exception {
        writeAscii("{\"record_type\":\"summary\",\"complete\":true,\"counts\":{");
        writeAscii("\"sms\":" + smsCount + ",\"mms\":" + mmsCount + ",\"mms_parts\":" + partCount);
        writeAscii(",\"mms_part_data\":" + partDataCount + ",\"mms_addresses\":" + addressCount);
        writeAscii(",\"threads\":" + threadCount + ",\"canonical_addresses\":" + canonicalAddressCount + "}}\n");
    }

    private ProviderHandle acquire(Object uri) throws Exception {
        String authority = (String) uriGetAuthority.invoke(uri);
        Object token = Class.forName("android.os.Binder").newInstance();
        Object holder = acquireProvider.invoke(activityManager, authority, Integer.valueOf(0), token, "*gmcli*");
        if (holder == null) throw new IllegalStateException("could not find provider " + authority);
        Object provider = holder.getClass().getField("provider").get(holder);
        if (provider == null) throw new IllegalStateException("provider holder is empty for " + authority);
        return new ProviderHandle(authority, token, provider);
    }

    private final class ProviderHandle {
        final String authority;
        final Object token;
        final Object provider;
        ProviderHandle(String authority, Object token, Object provider) {
            this.authority = authority;
            this.token = token;
            this.provider = provider;
        }
        void close() throws Exception {
            releaseProvider.invoke(activityManager, authority, token, Integer.valueOf(0));
        }
    }

    private void writeJSONString(String value) throws Exception {
        if (value == null) {
            writeAscii("null");
            return;
        }
        writeAscii("\"");
        for (int i = 0; i < value.length(); i++) {
            char ch = value.charAt(i);
            switch (ch) {
                case '\"': writeAscii("\\\""); break;
                case '\\': writeAscii("\\\\"); break;
                case '\b': writeAscii("\\b"); break;
                case '\f': writeAscii("\\f"); break;
                case '\n': writeAscii("\\n"); break;
                case '\r': writeAscii("\\r"); break;
                case '\t': writeAscii("\\t"); break;
                default:
                    if (ch < 0x20 || ch > 0x7e) {
                        writeAscii(String.format("\\u%04x", Integer.valueOf(ch)));
                    } else {
                        out.write((byte) ch);
                    }
            }
        }
        writeAscii("\"");
    }

    private void writeAscii(String value) throws Exception {
        out.write(value.getBytes("US-ASCII"));
    }

    private static String hex(byte[] bytes) {
        char[] chars = new char[bytes.length * 2];
        final char[] digits = "0123456789abcdef".toCharArray();
        for (int i = 0; i < bytes.length; i++) {
            int v = bytes[i] & 0xff;
            chars[i * 2] = digits[v >>> 4];
            chars[i * 2 + 1] = digits[v & 15];
        }
        return new String(chars);
    }

    private static String base64(byte[] data) {
        if (data == null || data.length == 0) return "";
        StringBuilder result = new StringBuilder(((data.length + 2) / 3) * 4);
        final char[] alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/".toCharArray();
        for (int i = 0; i < data.length; i += 3) {
            int a = data[i] & 0xff;
            int b = i + 1 < data.length ? data[i + 1] & 0xff : 0;
            int c = i + 2 < data.length ? data[i + 2] & 0xff : 0;
            result.append(alphabet[a >>> 2]);
            result.append(alphabet[((a & 3) << 4) | (b >>> 4)]);
            result.append(i + 1 < data.length ? alphabet[((b & 15) << 2) | (c >>> 6)] : '=');
            result.append(i + 2 < data.length ? alphabet[c & 63] : '=');
        }
        return result.toString();
    }

    private static final class Base64Stream {
        private static final byte[] ALPHABET = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/".getBytes();
        private final OutputStream out;
        private final byte[] pending = new byte[3];
        private int pendingCount;

        Base64Stream(OutputStream out) { this.out = out; }

        void write(byte[] data, int offset, int length) throws Exception {
            int end = offset + length;
            while (offset < end) {
                pending[pendingCount++] = data[offset++];
                if (pendingCount == 3) emit(3);
            }
        }

        void finish() throws Exception {
            if (pendingCount != 0) emit(pendingCount);
        }

        private void emit(int count) throws Exception {
            int a = pending[0] & 0xff;
            int b = count > 1 ? pending[1] & 0xff : 0;
            int c = count > 2 ? pending[2] & 0xff : 0;
            out.write(ALPHABET[a >>> 2]);
            out.write(ALPHABET[((a & 3) << 4) | (b >>> 4)]);
            out.write(count > 1 ? ALPHABET[((b & 15) << 2) | (c >>> 6)] : '=');
            out.write(count > 2 ? ALPHABET[c & 63] : '=');
            pendingCount = 0;
        }
    }
}
